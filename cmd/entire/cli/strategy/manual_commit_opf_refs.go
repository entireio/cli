// Pre-push OPF rewrite for the git-refs checkpoint backend, the sibling of
// manual_commit_opf_rewrite.go's entire/checkpoints/v1 rewrite. Beyond
// discovery and ref update, the backends differ in scope: this one redacts one
// queued ref per OPF call, because separate refs are independent chains, while
// the v1 rewrite must take its whole unpushed chain as a unit (each rebuilt
// commit is the next one's parent). That difference is documented at the v1
// rewrite's own cap check.
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
// Each ref is collected, cap-checked, scanned, rebuilt and CAS-updated before
// the next ref is loaded. Both the raw-memory ceiling and the prose-leaf
// inference cap therefore apply per ref. A ref that exceeds either is left
// untouched and still queued rather than failing the refs beside it.
//
// That isolation is not unconditional. Two conditions stop the whole flush and
// leave every ref after them untouched: an empty effective category set, and a
// tripped OPF circuit breaker — both mean OPF cannot scan anything, so no
// remaining ref may be stamped applied. See the breaker check in the loop.
//
// Caller checks redact.OPFEnabled() and skips this when OPF is off. Returns
// the same error taxonomy as RewriteUnpushedV1WithOPF; delivery fails closed by
// withholding any ref whose current generation still lacks the trailer.
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

	batchLimit := resolveBatchLimit()
	rawCap := rawByteCapForBatchLimit(batchLimit)
	bootstrapLimit := resolveBootstrapLimit()
	// Stale entries (refs no longer present locally) are skipped, not pruned:
	// the queue belongs to the flush.
	existing, _ := partitionLocalRefs(repo, queued)
	var firstErr error
	for _, refName := range existing {
		// Whole-flush stop, alongside ErrOPFNoEnabledCategories below: a
		// tripped process-wide breaker (this loop's own prior ref, or anything
		// earlier in the process) makes BatchBytesWithPrivacyFilter return
		// regex-only content with a NIL error, so break — not continue. Skipping
		// only this ref would rebuild every later one from content OPF never saw
		// and stamp Entire-OPF-Applied on it. Per-ref isolation covers per-ref
		// conditions (the size cap); a broken runtime is broken for the whole
		// process. Refs already rewritten above were scanned before the trip.
		if redact.OPFBreakerTripped() {
			if firstErr == nil {
				firstErr = &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand()}
			}
			break
		}

		pending, collectErr := collectCheckpointRefForOPF(repo, refName, rawCap, bootstrapLimit)
		if collectErr != nil {
			if errors.Is(collectErr, errNoCheckpointRefOPFWork) {
				continue
			}
			if firstErr == nil {
				firstErr = collectErr
			}
			continue
		}
		rewriteErr := rewriteCollectedCheckpointRefWithOPF(ctx, repo, queue, pending, batchLimit)
		if rewriteErr == nil {
			continue
		}
		if errors.Is(rewriteErr, redact.ErrOPFNoEnabledCategories) {
			return &OPFNoCategoriesError{}
		}
		if firstErr == nil {
			firstErr = rewriteErr
		}
		var runtimeErr *OPFRuntimeFailedError
		if errors.As(rewriteErr, &runtimeErr) {
			break
		}
	}
	return firstErr
}

type pendingOPFCommit struct {
	commit *object.Commit
	// blobs and paths are parallel and index the same redaction results.
	blobs []redact.NamedBlob
	paths []string
}

type pendingOPFRef struct {
	ref plumbing.ReferenceName
	old plumbing.Hash
	// base is the parent the deepest rewritten commit keeps.
	base    plumbing.Hash
	commits []pendingOPFCommit // ancestor-first
}

var errNoCheckpointRefOPFWork = errors.New("checkpoint ref does not need OPF rewrite")

func collectCheckpointRefForOPF(
	repo *git.Repository,
	refName plumbing.ReferenceName,
	rawCap, bootstrapLimit int,
) (*pendingOPFRef, error) {
	ref, err := repo.Reference(refName, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, errNoCheckpointRefOPFWork
		}
		return nil, fmt.Errorf("resolve checkpoint ref %s: %w", refName, err)
	}
	chain, base, err := unappliedAncestry(repo, ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("walk ancestry of %s: %w", refName, err)
	}
	if len(chain) == 0 {
		return nil, errNoCheckpointRefOPFWork
	}
	if len(chain) > bootstrapLimit {
		return nil, &BootstrapTooLargeError{Count: len(chain), Limit: bootstrapLimit}
	}

	pending := &pendingOPFRef{ref: refName, old: ref.Hash(), base: base}
	rawBytes := 0
	for _, commit := range chain {
		tree, treeErr := repo.TreeObject(commit.TreeHash)
		if treeErr != nil {
			return nil, fmt.Errorf("load tree for %s: %w", commit.Hash.String()[:7], treeErr)
		}
		pc := pendingOPFCommit{commit: commit}
		if err := collectTreeBlobs(repo, tree, "", &pc.blobs, &pc.paths); err != nil {
			return nil, fmt.Errorf("collect blobs %s: %w", commit.Hash.String()[:7], err)
		}
		for _, blob := range pc.blobs {
			rawBytes += len(blob.Content)
		}
		if rawBytes > rawCap {
			return nil, &OPFRawBytesTooLargeError{RawBytes: rawBytes, Limit: rawCap}
		}
		pending.commits = append(pending.commits, pc)
	}
	return pending, nil
}

func rewriteCollectedCheckpointRefWithOPF(
	ctx context.Context,
	repo *git.Repository,
	queue *checkpoint.PushQueue,
	pending *pendingOPFRef,
	batchLimit int,
) error {
	var blobs []redact.NamedBlob
	for _, commit := range pending.commits {
		blobs = append(blobs, commit.blobs...)
	}
	if leafBytes := redact.SumProseLeafBytes(blobs); leafBytes > batchLimit {
		return &OPFBatchTooLargeError{LeafBytes: leafBytes, Limit: batchLimit}
	}

	var redacted [][]byte
	if len(blobs) > 0 {
		var err error
		redacted, err = redact.BatchBytesWithPrivacyFilter(ctx, blobs)
		if err != nil {
			if errors.Is(err, redact.ErrOPFNoEnabledCategories) {
				return fmt.Errorf("scan checkpoint ref %s: %w", pending.ref, err)
			}
			return &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand(), Cause: err}
		}
	}

	// Replay ancestor→tip. A partial rebuild only creates unreachable objects;
	// the ref moves once, after the whole replacement chain exists.
	parent := pending.base
	startIdx := 0
	for _, commit := range pending.commits {
		redactedByPath := make(map[string][]byte, len(commit.blobs))
		for j, path := range commit.paths {
			redactedByPath[path] = redacted[startIdx+j]
		}
		startIdx += len(commit.blobs)
		newHash, err := rebuildCheckpointCommit(ctx, repo, commit.commit, parent, redactedByPath)
		if err != nil {
			return fmt.Errorf("rebuild checkpoint commit %s on %s: %w", commit.commit.Hash.String()[:7], pending.ref, err)
		}
		parent = newHash
	}

	// CAS prevents a concurrent checkpoint generation from being overwritten;
	// EnqueueRef then replaces the queue token with the rewritten generation.
	return updateOPFRewrittenRef(ctx, repo, queue, pending.ref, parent, pending.old)
}

func updateOPFRewrittenRef(
	ctx context.Context,
	repo *git.Repository,
	queue *checkpoint.PushQueue,
	refName plumbing.ReferenceName,
	newHash, oldHash plumbing.Hash,
) error {
	if err := checkpoint.CASPersistentRef(ctx, repo, refName, newHash, oldHash); err != nil {
		return fmt.Errorf("update checkpoint ref %s: %w", refName, err)
	}
	if err := queue.EnqueueRef(repo, refName); err != nil {
		return fmt.Errorf("enqueue rewritten checkpoint ref %s: %w", refName, err)
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
