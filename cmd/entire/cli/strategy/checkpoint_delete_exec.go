package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// DeleteOutcome is what happened to one copy of a checkpoint.
type DeleteOutcome string

const (
	DeleteOutcomeDeleted DeleteOutcome = "deleted"
	// DeleteOutcomeAbsent: there was nothing to delete there (already gone).
	DeleteOutcomeAbsent DeleteOutcome = "absent"
	DeleteOutcomeFailed DeleteOutcome = "failed"
	// DeleteOutcomeSkipped: not attempted (not selected, or not applicable).
	DeleteOutcomeSkipped DeleteOutcome = "skipped"
)

// CheckpointDeleteOptions selects what ExecuteCheckpointDelete touches.
type CheckpointDeleteOptions struct {
	// Targets are the remote targets to delete from (a subset of the plan's).
	Targets []CheckpointDeleteTarget
	// LocalOnly skips every remote.
	LocalOnly bool
}

// CheckpointDeleteTargetResult is the outcome on one remote target.
type CheckpointDeleteTargetResult struct {
	Target CheckpointDeleteTarget
	Ref    DeleteOutcome
	V1     DeleteOutcome
	Error  string
}

// Failed reports whether anything on this target failed.
func (r CheckpointDeleteTargetResult) Failed() bool {
	return r.Ref == DeleteOutcomeFailed || r.V1 == DeleteOutcomeFailed
}

// CheckpointDeleteResult reports every step of a delete.
type CheckpointDeleteResult struct {
	StatesCleared []string
	LocalRef      DeleteOutcome
	LocalV1       DeleteOutcome
	Targets       []CheckpointDeleteTargetResult
}

// Failed reports whether any requested remote delete failed.
func (r *CheckpointDeleteResult) Failed() bool {
	for _, t := range r.Targets {
		if t.Failed() {
			return true
		}
	}
	return false
}

// errNothingToRemove stops a v1 rebuild whose tree no longer has the subtree.
var errNothingToRemove = errors.New("checkpoint subtree already absent")

// ExecuteCheckpointDelete deletes the planned checkpoint.
//
// Order matters. Session states are cleared first so no local writer (an
// amend restoring the trailer, a pending condensation) recreates the ID, and
// the ID is recorded on the deleted list before any ref goes. The local ref is
// then removed before any remote one: a concurrent pre-push drains the queue
// and pushes refs that exist locally, so deleting remote-first would let it
// push the ref back between the two deletes. Remote failures are reported per
// target and never restore the local copy; the user retries with --remote.
func ExecuteCheckpointDelete(ctx context.Context, plan *CheckpointDeletePlan, opts CheckpointDeleteOptions) (_ *CheckpointDeleteResult, retErr error) {
	if err := plan.CheckLocalV1Propagation(opts.Targets, opts.LocalOnly); err != nil {
		return nil, err
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	ctx = settings.WithWorktreeRoot(ctx, root)
	cid := plan.CheckpointID
	result := &CheckpointDeleteResult{LocalRef: DeleteOutcomeAbsent, LocalV1: DeleteOutcomeAbsent}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return result, err
	}
	defer repo.Close()

	// Recorded first, so a hook racing the steps below (an amend, a
	// migration) already declines the ID.
	deletedList, err := checkpoint.DeletedCheckpointsForRepo(repo)
	if err != nil {
		return result, fmt.Errorf("record deleted checkpoint: %w", err)
	}
	if err := deletedList.Record(cid); err != nil {
		return result, fmt.Errorf("record deleted checkpoint: %w", err)
	}
	// A delete that fails before removing any copy must not leave the ID
	// listed: hooks would strip the trailer of a checkpoint that still exists.
	copyDeleted := false
	defer func() {
		if retErr == nil || copyDeleted {
			return
		}
		if err := deletedList.Remove(cid); err != nil {
			logging.Warn(ctx, "checkpoint delete: could not unrecord a delete that removed nothing",
				slog.String("checkpoint_id", cid.String()), slog.String("error", err.Error()))
		}
	}()

	for _, sessionID := range sessionsHoldingCheckpoint(ctx, plan) {
		changed := false
		if err := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
			if !clearDeletedCheckpointFromState(state, cid) {
				return ErrMutationSkip
			}
			changed = true
			return nil
		}); err != nil && !errors.Is(err, ErrStateNotFound) {
			return result, fmt.Errorf("clear checkpoint from session %s: %w", sessionID, err)
		}
		if changed {
			result.StatesCleared = append(result.StatesCleared, sessionID)
		}
	}

	refDeleted, err := deleteLocalCheckpointRef(ctx, root, repo, plan)
	copyDeleted = refDeleted
	if refDeleted {
		result.LocalRef = DeleteOutcomeDeleted
	}
	if err != nil {
		result.LocalRef = DeleteOutcomeFailed
		return result, err
	}

	if plan.LocalV1 {
		removed, err := removeLocalV1Checkpoint(ctx, repo, cid)
		if err != nil {
			result.LocalV1 = DeleteOutcomeFailed
			return result, fmt.Errorf("remove checkpoint from local %s: %w", paths.MetadataBranchName, err)
		}
		if removed {
			result.LocalV1 = DeleteOutcomeDeleted
			copyDeleted = true
		}
	}

	if opts.LocalOnly {
		return result, nil
	}
	for _, target := range opts.Targets {
		result.Targets = append(result.Targets, deleteFromTarget(ctx, root, cid, target))
	}
	recheckRemoteRefs(ctx, root, cid, result)
	return result, nil
}

// sessionsHoldingCheckpoint returns the planned session states plus any that
// started referencing the ID while the confirmation prompt was open.
func sessionsHoldingCheckpoint(ctx context.Context, plan *CheckpointDeletePlan) []string {
	ids := make([]string, 0, len(plan.SessionStates))
	for _, st := range plan.SessionStates {
		ids = append(ids, st.SessionID)
	}
	fresh := &CheckpointDeletePlan{CheckpointID: plan.CheckpointID}
	if err := collectDeleteStates(ctx, fresh); err != nil {
		logging.Debug(ctx, "checkpoint delete: could not re-list session states", slog.String("error", err.Error()))
		return ids
	}
	for _, st := range fresh.SessionStates {
		if !slices.Contains(ids, st.SessionID) {
			ids = append(ids, st.SessionID)
		}
	}
	return ids
}

// deleteLocalCheckpointRef deletes the local ref with a compare-and-swap on
// the planned oid, then drops it from the push queue, reporting whether the
// ref was deleted. The queue entry goes even when the ref is already absent,
// so a stale entry cannot linger.
func deleteLocalCheckpointRef(ctx context.Context, root string, repo *git.Repository, plan *CheckpointDeletePlan) (bool, error) {
	deleted := false
	if plan.LocalRef != "" {
		if err := gitrepo.CompareAndSwapRef(ctx, root, plan.LocalRef, plumbing.ZeroHash, plan.LocalRefOID); err != nil {
			return false, fmt.Errorf("delete local ref %s: %w", plan.LocalRef, err)
		}
		deleted = true
	}
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return deleted, err //nolint:wrapcheck // already names the push queue
	}
	refs := []plumbing.ReferenceName{}
	if plan.LocalRef != "" {
		refs = append(refs, plan.LocalRef)
	}
	if canonical, nameErr := checkpoint.RefName(plan.CheckpointID); nameErr == nil && canonical != plan.LocalRef {
		refs = append(refs, canonical)
	}
	if err := queue.Remove(refs); err != nil {
		return deleted, fmt.Errorf("remove checkpoint from push queue: %w", err)
	}
	return deleted, nil
}

// removeLocalV1Checkpoint commits the subtree's removal onto the local v1
// branch under the checkpoint writers' lock, rebuilding on a CAS conflict.
func removeLocalV1Checkpoint(ctx context.Context, repo *git.Repository, cid id.CheckpointID) (bool, error) {
	err := checkpoint.UpdatePersistentRef(ctx, repo, v1BranchRef, func() (plumbing.Hash, plumbing.Hash, error) {
		tip := refTip(repo, v1BranchRef)
		if tip.IsZero() {
			return plumbing.ZeroHash, plumbing.ZeroHash, errNothingToRemove
		}
		commit, err := buildCheckpointRemovalCommit(ctx, repo, tip, cid)
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		return commit, tip, nil
	})
	switch {
	case errors.Is(err, errNothingToRemove):
		return false, nil
	case err != nil:
		return false, err //nolint:wrapcheck // caller adds the branch context
	}
	return true, nil
}

// buildCheckpointRemovalCommit writes a commit on parent whose tree lacks cid's
// subtree. It returns errNothingToRemove when the subtree is already gone.
func buildCheckpointRemovalCommit(ctx context.Context, repo *git.Repository, parent plumbing.Hash, cid id.CheckpointID) (plumbing.Hash, error) {
	parentCommit, err := repo.CommitObject(parent)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read commit %s: %w", parent, err)
	}
	newTree, removed, err := checkpoint.RemoveCheckpointSubtree(repo, parentCommit.TreeHash, cid)
	if err != nil {
		return plumbing.ZeroHash, err //nolint:wrapcheck // names the checkpoint already
	}
	if !removed {
		return plumbing.ZeroHash, errNothingToRemove
	}
	if newTree.IsZero() {
		if newTree, err = checkpoint.BuildTreeFromEntries(ctx, repo, nil); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("build empty tree: %w", err)
		}
	}
	name, email := checkpoint.GetGitAuthorFromRepo(repo)
	commit, err := checkpoint.CreateCommit(ctx, repo, newTree, parent, "Delete checkpoint "+cid.String(), name, email)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit checkpoint removal: %w", err)
	}
	return commit, nil
}

// deleteFromTarget deletes the per-checkpoint ref and the v1 copy on one target.
func deleteFromTarget(ctx context.Context, root string, cid id.CheckpointID, target CheckpointDeleteTarget) CheckpointDeleteTargetResult {
	res := CheckpointDeleteTargetResult{Target: target, Ref: DeleteOutcomeAbsent, V1: DeleteOutcomeAbsent}
	var errs []string
	if !target.RefOID.IsZero() {
		outcome, reason := deleteRemoteRef(ctx, root, target.URL, cid, target.RefName, target.RefOID)
		res.Ref = outcome
		if reason != "" {
			errs = append(errs, reason)
		}
	}
	if !target.V1Tip.IsZero() && target.V1 != V1CopyAbsent {
		outcome, reason := deleteFromRemoteV1(ctx, root, cid, target)
		res.V1 = outcome
		if reason != "" {
			errs = append(errs, reason)
		}
	}
	res.Error = strings.Join(errs, "; ")
	return res
}

// deleteRemoteRef runs `git push --force-with-lease=<ref>:<oid> <target> :<ref>`.
// The lease keeps a copy another clone pushed since the probe from being
// removed unseen. A lease that fails because the ref is gone is success: the
// same repository can be reached under two spellings (a push URL and a fetch
// URL), and the first target's delete makes the second one's lease stale.
func deleteRemoteRef(ctx context.Context, root, target string, cid id.CheckpointID, ref plumbing.ReferenceName, oid plumbing.Hash) (DeleteOutcome, string) {
	res, err := remote.PushWithOptions(ctx, remote.PushOptions{
		Remote:    target,
		RefSpecs:  []string{":" + ref.String()},
		ExtraArgs: []string{"--force-with-lease=" + ref.String() + ":" + oid.String()},
		Dir:       root,
	})
	outcome, reason := classifyRemoteDelete(res.Output, err)
	if outcome == DeleteOutcomeFailed && isStaleLease(res.Output) {
		if listing, lsErr := lsRemoteCheckpoint(ctx, root, target, cid); lsErr == nil && listing.refOID.IsZero() {
			return DeleteOutcomeAbsent, ""
		}
	}
	if outcome == DeleteOutcomeFailed {
		logging.Warn(ctx, "checkpoint delete: remote ref delete failed",
			slog.String("target", remote.RedactURLOrPath(target)),
			slog.String("reason", reason))
	}
	return outcome, reason
}

var rejectedReasonPattern = regexp.MustCompile(`\[(?:remote )?rejected\]\s*\(([^)]*)\)`)

// classifyRemoteDelete maps a deleting push's porcelain output to an outcome.
// A ref the remote no longer has counts as deleted-already; a lease mismatch
// ("stale info") means another clone changed it; a hook or protection
// rejection carries the remote's reason.
func classifyRemoteDelete(output string, pushErr error) (DeleteOutcome, string) {
	if pushErr == nil {
		return DeleteOutcomeDeleted, ""
	}
	switch {
	case strings.Contains(output, "remote ref does not exist"):
		return DeleteOutcomeAbsent, ""
	case isStaleLease(output):
		return DeleteOutcomeFailed, "the remote copy changed since it was inspected (another clone may have pushed it); run the delete again"
	}
	if m := rejectedReasonPattern.FindStringSubmatch(output); m != nil {
		return DeleteOutcomeFailed, "rejected by the remote: " + m[1]
	}
	return DeleteOutcomeFailed, pushErr.Error()
}

func isStaleLease(output string) bool {
	return strings.Contains(output, "stale info")
}

// deleteFromRemoteV1 removes cid's subtree from target's v1 branch: it fetches
// the target's tip into a temporary ref, commits the removal on that tip, and
// pushes with a lease on the fetched tip. Local v1 commits are never pushed, so
// unpushed (for example not yet OPF-redacted) checkpoint data stays local.
func deleteFromRemoteV1(ctx context.Context, root string, cid id.CheckpointID, target CheckpointDeleteTarget) (DeleteOutcome, string) {
	tmpRef := plumbing.ReferenceName(FetchTmpRefPrefix + "checkpoint-delete/" + cid.String())
	if err := fetchURLIntoTmpRef(ctx, root, target.URL, v1BranchRef.String(), tmpRef.String(), "checkpoint branch", true, checkpointRemoteForegroundFetchTimeout); err != nil {
		return DeleteOutcomeFailed, err.Error()
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return DeleteOutcomeFailed, err.Error()
	}
	defer repo.Close()
	tip := refTip(repo, tmpRef)
	defer func() {
		if !tip.IsZero() {
			_ = gitrepo.CompareAndSwapRef(ctx, root, tmpRef, plumbing.ZeroHash, tip) //nolint:errcheck // temp ref cleanup is best-effort
		}
	}()
	if tip.IsZero() {
		return DeleteOutcomeFailed, "fetched checkpoint branch is missing"
	}

	commit, err := buildCheckpointRemovalCommit(ctx, repo, tip, cid)
	if errors.Is(err, errNothingToRemove) {
		// Already gone there (for example deleted through another spelling
		// of the same URL): the tracking ref still has to stop showing it.
		advanceTrackingV1(ctx, root, repo, target, tip)
		return DeleteOutcomeAbsent, ""
	}
	if err != nil {
		return DeleteOutcomeFailed, err.Error()
	}
	res, pushErr := remote.PushWithOptions(ctx, remote.PushOptions{
		Remote:    target.URL,
		RefSpecs:  []string{commit.String() + ":" + v1BranchRef.String()},
		ExtraArgs: []string{"--force-with-lease=" + v1BranchRef.String() + ":" + tip.String()},
		Dir:       root,
	})
	if pushErr != nil {
		outcome, reason := classifyRemoteDelete(res.Output, pushErr)
		if outcome != DeleteOutcomeFailed {
			outcome, reason = DeleteOutcomeFailed, pushErr.Error()
		}
		return outcome, reason
	}
	advanceTrackingV1(ctx, root, repo, target, commit)
	return DeleteOutcomeDeleted, ""
}

// advanceTrackingV1 moves each named remote's existing v1 tracking ref to tip,
// a v1 commit of the target without the checkpoint, so read fallbacks and
// migration stop seeing it. Only remotes that FETCH from the target are moved:
// a remote that matched the target through its push URL may track a different
// repository. When the push URL and fetch URL are two spellings of one
// repository, the fetch-URL target moves the ref, even when its own delete
// found the checkpoint already gone.
func advanceTrackingV1(ctx context.Context, root string, repo *git.Repository, target CheckpointDeleteTarget, tip plumbing.Hash) {
	for _, name := range target.Remotes {
		if name == CheckpointRemoteTargetName {
			continue
		}
		if fetchURL, err := remote.GetRemoteURL(ctx, name); err != nil || fetchURL != target.URL {
			continue
		}
		tracking := plumbing.NewRemoteReferenceName(name, paths.MetadataBranchName)
		current := refTip(repo, tracking)
		if current.IsZero() || current == tip {
			continue
		}
		if err := gitrepo.CompareAndSwapRef(ctx, root, tracking, tip, current); err != nil {
			logging.Warn(ctx, "checkpoint delete: could not advance tracking ref",
				slog.String("ref", tracking.String()), slog.String("error", err.Error()))
		}
	}
}

// recheckRemoteRefs probes each target that did not fail once more. A
// pre-push that drained the queue before the local delete can still push the
// ref back; one retry, leased on the oid it pushed, removes that copy.
func recheckRemoteRefs(ctx context.Context, root string, cid id.CheckpointID, result *CheckpointDeleteResult) {
	for i := range result.Targets {
		r := &result.Targets[i]
		if r.Ref == DeleteOutcomeFailed {
			continue
		}
		listing, err := lsRemoteCheckpoint(ctx, root, r.Target.URL, cid)
		if err != nil || listing.refOID.IsZero() {
			continue
		}
		outcome, reason := deleteRemoteRef(ctx, root, r.Target.URL, cid, listing.ref, listing.refOID)
		if outcome == DeleteOutcomeDeleted {
			r.Ref = DeleteOutcomeDeleted
		}
		if outcome == DeleteOutcomeFailed {
			r.Ref = DeleteOutcomeFailed
			msg := "the checkpoint was pushed again while it was being deleted: " + reason
			if r.Error != "" {
				msg = r.Error + "; " + msg
			}
			r.Error = msg
		}
	}
}
