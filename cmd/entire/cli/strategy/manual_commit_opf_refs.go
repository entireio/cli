// Pre-push OPF rewrite for the git-refs checkpoint backend, the sibling of
// manual_commit_opf_rewrite.go's entire/checkpoints/v1 rewrite. Both run the
// OPF-augmented redaction once per push; only discovery and ref update differ.
package strategy

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// RewriteQueuedCheckpointRefsWithOPF re-redacts the checkpoint refs awaiting
// push with OPF, rebuilds every un-applied commit on each as a commit carrying
// Entire-OPF-Applied: true, and points the ref at the new tip. Idempotent: a
// ref whose tip already carries the trailer is left byte-identical.
//
// Discovery is the push-discovery queue rather than a local-vs-remote diff: it
// already names exactly the refs this push will carry, so there is no
// merge-base or divergence analysis. Within a ref, pushing it carries its whole
// unpushed ancestry, so the walk covers every commit back to the first one
// already carrying the trailer (unappliedAncestry) — bounded by the same
// resolveBootstrapLimit the v1 path uses.
//
// Caller checks redact.OPFEnabled() and skips this when OPF is off. Returns
// the same error taxonomy as RewriteUnpushedV1WithOPF; the caller fails closed
// by withholding the flush (see prePushCheckpointRefs).
func RewriteQueuedCheckpointRefsWithOPF(ctx context.Context, repo *git.Repository) error {
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return fmt.Errorf("resolve push queue: %w", err)
	}
	// Peek, not Drain: flushCheckpointRefsQueue owns draining and pruning.
	queued, err := queue.Peek()
	if err != nil {
		return fmt.Errorf("peek push queue: %w", err)
	}
	if len(queued) == 0 {
		return nil
	}

	// Both up-front fail-closed gates, in the same order as the v1 rewrite, so
	// a misconfigured category set surfaces config remediation rather than
	// "verify your OPF install".
	if redact.OPFMisconfiguredNoCategories() {
		return &OPFNoCategoriesError{}
	}
	if redact.OPFBreakerTripped() {
		return &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand()}
	}

	// Pass 1: collect every redactable blob from every un-applied commit on every
	// queued ref, bounding raw bytes in memory exactly as the v1 collect pass
	// does — cumulatively across the whole flush, not per ref.
	type pendingCommit struct {
		commit *object.Commit
		// blobs and paths are parallel; startIdx is this commit's offset into
		// the global redacted slice.
		blobs    []redact.NamedBlob
		paths    []string
		startIdx int
	}
	type pendingRef struct {
		ref plumbing.ReferenceName
		old plumbing.Hash
		// base is the parent the deepest rewritten commit keeps.
		base    plumbing.Hash
		commits []pendingCommit // ancestor-first
	}
	var globalBlobs []redact.NamedBlob
	pendings := make([]pendingRef, 0, len(queued))
	rawCap := scaleBatchLimit(resolveBatchLimit(), rawByteCapMultiplier)
	bootstrapLimit := resolveBootstrapLimit()
	var rawBytesSoFar int
	// Stale entries (refs no longer present locally) are skipped, not pruned:
	// the queue belongs to the flush.
	existing, _ := partitionLocalRefs(repo, queued)
	for _, refName := range existing {
		ref, refErr := repo.Reference(refName, true)
		if refErr != nil {
			if errors.Is(refErr, plumbing.ErrReferenceNotFound) {
				continue
			}
			return fmt.Errorf("resolve checkpoint ref %s: %w", refName, refErr)
		}
		chain, base, walkErr := unappliedAncestry(repo, ref.Hash())
		if walkErr != nil {
			return fmt.Errorf("walk ancestry of %s: %w", refName, walkErr)
		}
		if len(chain) == 0 {
			continue
		}
		if len(chain) > bootstrapLimit {
			return &BootstrapTooLargeError{Count: len(chain), Limit: bootstrapLimit}
		}
		pr := pendingRef{ref: refName, old: ref.Hash(), base: base}
		for _, c := range chain {
			tree, treeErr := repo.TreeObject(c.TreeHash)
			if treeErr != nil {
				return fmt.Errorf("load tree for %s: %w", c.Hash.String()[:7], treeErr)
			}
			pc := pendingCommit{commit: c, startIdx: len(globalBlobs)}
			if err := collectTreeBlobs(repo, tree, "", &pc.blobs, &pc.paths); err != nil {
				return fmt.Errorf("collect blobs %s: %w", c.Hash.String()[:7], err)
			}
			for _, b := range pc.blobs {
				rawBytesSoFar += len(b.Content)
			}
			if rawBytesSoFar > rawCap {
				return &OPFRawBytesTooLargeError{RawBytes: rawBytesSoFar, Limit: rawCap}
			}
			globalBlobs = append(globalBlobs, pc.blobs...)
			pr.commits = append(pr.commits, pc)
		}
		pendings = append(pendings, pr)
	}
	if len(pendings) == 0 {
		return nil
	}

	// Pass 2: enforce the leaf-byte cap, then make exactly ONE OPF shell-out
	// for the whole flush.
	var globalRedacted [][]byte
	if len(globalBlobs) > 0 {
		leafBytes := redact.SumProseLeafBytes(globalBlobs)
		if limit := resolveBatchLimit(); leafBytes > limit {
			return &OPFBatchTooLargeError{LeafBytes: leafBytes, Limit: limit}
		}
		globalRedacted, err = redact.BatchBytesWithPrivacyFilter(ctx, globalBlobs)
		if err != nil {
			if errors.Is(err, redact.ErrOPFNoEnabledCategories) {
				return &OPFNoCategoriesError{}
			}
			return &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand(), Cause: err}
		}
	}

	// Pass 3: rebuild every commit before touching any ref, so a failure
	// part-way through leaves every ref where it was. Each ref's chain is
	// replayed ancestor→tip, so the rewritten parent carries into the next
	// commit and the deepest one keeps the boundary parent.
	rebuilt := make([]plumbing.Hash, len(pendings))
	for i, pr := range pendings {
		parent := pr.base
		for _, pc := range pr.commits {
			redactedByPath := make(map[string][]byte, len(pc.blobs))
			for j, path := range pc.paths {
				redactedByPath[path] = globalRedacted[pc.startIdx+j]
			}
			newHash, rebuildErr := rebuildCheckpointCommit(ctx, repo, pc.commit, parent, redactedByPath)
			if rebuildErr != nil {
				return fmt.Errorf("rebuild checkpoint commit %s on %s: %w", pc.commit.Hash.String()[:7], pr.ref, rebuildErr)
			}
			parent = newHash
		}
		rebuilt[i] = parent
	}

	// CAS each ref: a concurrent write that advanced a checkpoint ref during
	// the rewrite must not be clobbered by our stale rebuild.
	for i, pr := range pendings {
		if err := checkpoint.CompareAndSwapRef(ctx, repo, pr.ref, rebuilt[i], pr.old); err != nil {
			return fmt.Errorf("update checkpoint ref %s: %w", pr.ref, err)
		}
	}
	return nil
}

// unappliedAncestry walks first parents back from a checkpoint ref's tip and
// returns the commits that do not carry the OPF trailer, ancestor-first, plus
// the parent the deepest one keeps: the trailered commit the walk stopped at,
// or zero at a root. Checkpoint refs are a single-parent chain, so the first
// parent is the whole history.
func unappliedAncestry(repo *git.Repository, tip plumbing.Hash) ([]*object.Commit, plumbing.Hash, error) {
	var chain []*object.Commit
	base := plumbing.ZeroHash
	for h := tip; !h.IsZero(); {
		c, err := repo.CommitObject(h)
		if err != nil {
			return nil, plumbing.ZeroHash, fmt.Errorf("load commit %s: %w", h.String()[:7], err)
		}
		if trailers.HasOPFApplied(c.Message) {
			base = h
			break
		}
		chain = append(chain, c)
		if len(c.ParentHashes) == 0 {
			break
		}
		h = c.ParentHashes[0]
	}
	slices.Reverse(chain)
	return chain, base, nil
}
