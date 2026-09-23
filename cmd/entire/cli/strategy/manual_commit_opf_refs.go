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
// Each ref is scanned, rebuilt and CAS-updated independently, so a ref whose
// prose-leaf bytes exceed the inference cap is skipped — left untouched and
// still queued — rather than failing the refs beside it. The first such failure
// is still returned, so the caller's flush is withheld exactly as before: the
// scoping isolates the rewrite, not the delivery.
//
// That isolation is not unconditional. Two conditions stop the whole flush and
// leave every ref after them untouched: an empty effective category set, and a
// tripped OPF circuit breaker — both mean OPF cannot scan anything, so no
// remaining ref may be stamped applied. See the breaker check in Pass 2.
//
// Caller checks redact.OPFEnabled() and skips this when OPF is off. Returns
// the same error taxonomy as RewriteUnpushedV1WithOPF; the caller fails closed
// by withholding the flush (see prePushCheckpointRefs).
//
// Follow-up (not done here): Pass 2's body sits at this file's comment-density
// ceiling — golangci's maintidx nearly failed on the breaker-check comment
// alone. Extracting that check into a small named helper (e.g.
// stopFlushOnBreakerTrip) would give the "second whole-flush-stop condition"
// explanation a home that doesn't compete with the loop's own narrative, and
// buy real headroom for the next legitimate addition. Don't resolve a future
// maintidx failure here by trimming a safety comment instead.
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
	// queued ref, keeping each ref's blobs separately addressable so Pass 2 can
	// scan one ref at a time. Raw bytes in memory are still bounded cumulatively
	// across the whole flush, exactly as the v1 collect pass does: that ceiling
	// is about this process's RAM, which every ref shares, not about the
	// per-ref inference cost the leaf-byte cap governs.
	type pendingCommit struct {
		commit *object.Commit
		// blobs and paths are parallel; blobs are indexed against their own
		// ref's redaction results, not a flush-wide slice.
		blobs []redact.NamedBlob
		paths []string
	}
	type pendingRef struct {
		ref plumbing.ReferenceName
		old plumbing.Hash
		// base is the parent the deepest rewritten commit keeps.
		base    plumbing.Hash
		commits []pendingCommit // ancestor-first
	}
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
			pc := pendingCommit{commit: c}
			if err := collectTreeBlobs(repo, tree, "", &pc.blobs, &pc.paths); err != nil {
				return fmt.Errorf("collect blobs %s: %w", c.Hash.String()[:7], err)
			}
			for _, b := range pc.blobs {
				rawBytesSoFar += len(b.Content)
			}
			if rawBytesSoFar > rawCap {
				return &OPFRawBytesTooLargeError{RawBytes: rawBytesSoFar, Limit: rawCap}
			}
			pr.commits = append(pr.commits, pc)
		}
		pendings = append(pendings, pr)
	}
	if len(pendings) == 0 {
		return nil
	}

	// Pass 2: take each ref on its own — enforce the leaf-byte cap over that
	// ref's blobs, make one OPF shell-out for them, replay its chain, and CAS
	// its ref — before moving to the next. A ref that trips the cap is skipped,
	// left exactly as it was and still queued for the next push, without
	// blocking any other ref in this flush. That scoping is the whole point:
	// the cap bounds one inference call's cost, and one oversized ref is no
	// reason to leave every ref beside it unredacted.
	//
	// The fail-closed contract holds at ref granularity: a ref is either fully
	// redacted, tagged and moved, or fully untouched. No ref's chain is ever
	// partially rewritten — within a ref, each rebuilt commit is the next
	// one's parent, so a chain that fails part-way through is abandoned whole.
	var firstErr error
	for _, pr := range pendings {
		// Second whole-flush stop, alongside ErrOPFNoEnabledCategories below:
		// a tripped process-wide breaker (this loop's own prior ref, or anything
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

		var refBlobs []redact.NamedBlob
		for _, pc := range pr.commits {
			refBlobs = append(refBlobs, pc.blobs...)
		}

		var refRedacted [][]byte
		if len(refBlobs) > 0 {
			leafBytes := redact.SumProseLeafBytes(refBlobs)
			if limit := resolveBatchLimit(); leafBytes > limit {
				if firstErr == nil {
					firstErr = &OPFBatchTooLargeError{LeafBytes: leafBytes, Limit: limit}
				}
				continue // leave this ref queued; the rest can still run
			}
			var opfErr error
			refRedacted, opfErr = redact.BatchBytesWithPrivacyFilter(ctx, refBlobs)
			if opfErr != nil {
				if errors.Is(opfErr, redact.ErrOPFNoEnabledCategories) {
					// Configuration-level failure: every ref fails identically,
					// so report the config remediation rather than retrying.
					return &OPFNoCategoriesError{}
				}
				if firstErr == nil {
					firstErr = &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand(), Cause: opfErr}
				}
				continue
			}
		}

		// Replay this ref's chain ancestor→tip, so the rewritten parent carries
		// into the next commit and the deepest one keeps the boundary parent.
		parent := pr.base
		startIdx := 0
		rebuildFailed := false
		for _, pc := range pr.commits {
			redactedByPath := make(map[string][]byte, len(pc.blobs))
			for j, path := range pc.paths {
				redactedByPath[path] = refRedacted[startIdx+j]
			}
			startIdx += len(pc.blobs)
			newHash, rebuildErr := rebuildCheckpointCommit(ctx, repo, pc.commit, parent, redactedByPath)
			if rebuildErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("rebuild checkpoint commit %s on %s: %w", pc.commit.Hash.String()[:7], pr.ref, rebuildErr)
				}
				rebuildFailed = true
				break
			}
			parent = newHash
		}
		if rebuildFailed {
			continue // the half-built chain is abandoned; the ref never moves
		}

		// CAS: a concurrent write that advanced this checkpoint ref during the
		// rewrite must not be clobbered by our stale rebuild.
		if err := updateOPFRewrittenRef(ctx, repo, queue, pr.ref, parent, pr.old); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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
