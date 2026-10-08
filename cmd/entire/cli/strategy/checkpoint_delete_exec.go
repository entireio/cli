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
	"github.com/go-git/go-git/v6/plumbing/object"

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
	// Selection says how Targets were chosen; it only shapes the wording of
	// a refusal. LocalOnly implies SelectionLocalOnly.
	Selection CheckpointDeleteSelection
}

func (o CheckpointDeleteOptions) selection() CheckpointDeleteSelection {
	if o.LocalOnly {
		return SelectionLocalOnly
	}
	return o.Selection
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
	if err := plan.CheckLocalV1Propagation(opts.Targets, opts.selection()); err != nil {
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
	addedHere, err := deletedList.Add(cid)
	if err != nil {
		return result, fmt.Errorf("record deleted checkpoint: %w", err)
	}
	// A delete that removed no copy (it failed early, or every remote
	// refused) must not leave the ID listed: hooks would strip the trailer of
	// a checkpoint that still exists. Only this run's own record is undone; an
	// ID an earlier, partly successful delete recorded stays.
	copyDeleted := false
	defer func() {
		if !addedHere || copyDeleted || result.anyRemoteDeleted() || !failuresLeftEveryCopy(ctx, root, cid, result) {
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

	var localRemoval plumbing.Hash
	if plan.LocalV1 {
		removal, removed, err := removeLocalV1Checkpoint(ctx, repo, cid)
		localRemoval = removal
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
	adopted := false
	for _, target := range opts.Targets {
		res, pushedV1 := deleteFromTarget(ctx, root, cid, target)
		result.Targets = append(result.Targets, res)
		if !adopted && !pushedV1.IsZero() && !localRemoval.IsZero() && slices.Contains(plan.V1PushURLs, target.URL) {
			adopted = true
			if err := adoptRemoteV1Removal(ctx, repo, cid, refTip(repo, v1BranchRef), localRemoval, pushedV1); err != nil {
				logging.Warn(ctx, "checkpoint delete: local v1 left as is; the next push reconciles it (with OPF on, it refuses until v1 is recovered)",
					slog.String("checkpoint_id", cid.String()), slog.String("reason", err.Error()))
			}
		}
	}
	recheckRemoteRefs(ctx, root, cid, result)
	return result, nil
}

// failuresLeftEveryCopy re-probes each failed target and reports whether the
// failure certainly removed nothing: the ref is still there and the v1 branch
// still has the tip the plan saw. A push that errored can still have landed
// (a dropped connection after the remote applied it), and an unreachable
// target cannot be checked, so either keeps the ID recorded.
func failuresLeftEveryCopy(ctx context.Context, root string, cid id.CheckpointID, result *CheckpointDeleteResult) bool {
	for _, r := range result.Targets {
		if !r.Failed() {
			continue
		}
		listing, err := lsRemoteCheckpoint(ctx, root, r.Target.URL, cid)
		if err != nil {
			return false
		}
		if r.Ref == DeleteOutcomeFailed && listing.refOID.IsZero() {
			return false
		}
		if r.V1 == DeleteOutcomeFailed && !listing.v1Tip.Equal(r.Target.V1Tip) {
			return false
		}
	}
	return true
}

func (r *CheckpointDeleteResult) anyRemoteDeleted() bool {
	return slices.ContainsFunc(r.Targets, func(t CheckpointDeleteTargetResult) bool {
		return t.Ref == DeleteOutcomeDeleted || t.V1 == DeleteOutcomeDeleted
	})
}

// errNotThisDeletesDivergence: local and remote v1 differ by more than this
// delete's two removal commits, so rebuilding local v1 would hide a divergence
// (a force-rewritten remote, say) that OPF's pre-push check exists to refuse.
var errNotThisDeletesDivergence = errors.New("local and remote v1 diverged before this delete")

// adoptRemoteV1Removal rebuilds the local v1 branch on pushed, the removal
// commit just pushed to a v1 push destination. The local removal
// (localRemoval) and the remote one are separate commits, so without this the
// branches diverge, and with OPF enabled a diverged v1 aborts the next push
// (V1DivergedError). It acts only when that pair is the whole divergence: the
// remote tip the removal was pushed on must already be in the local removal's
// history. Local commits after that tip are replayed onto pushed, except the
// local removal itself, whose change pushed already carries.
//
// Only the first v1 push destination the removal reached is adopted. With
// several pushurls the others received their own removal commits, so local v1
// can still diverge from them; the next pre-push reconciles those without
// OPF, and with OPF its divergence check refuses them as before.
//
// local is the v1 tip the rebuild starts from; the ref moves only if it still
// points there (one compare-and-swap, no retry). A lost race leaves local v1
// alone and never deletes an object: pushed already exists (the remote-tracking
// ref may point at it), and replayed commits left unreferenced are garbage git
// collects. Without OPF the next pre-push reconciles a diverged branch itself.
func adoptRemoteV1Removal(ctx context.Context, repo *git.Repository, cid id.CheckpointID, local, localRemoval, pushed plumbing.Hash) error {
	if local.IsZero() || local.Equal(pushed) {
		return nil
	}
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return err
	}
	if err := checkRemovalPairIsTheDivergence(ctx, repo, repoPath, local, localRemoval, pushed); err != nil {
		return err
	}
	commits, err := collectCommitsSince(ctx, repo, repoPath, local, pushed)
	if err != nil {
		return err
	}
	replay := slices.DeleteFunc(commits, func(c *object.Commit) bool { return c.Hash.Equal(localRemoval) })
	newTip := pushed
	if len(replay) > 0 {
		if newTip, err = replayOnto(ctx, repo, repoPath, cid, pushed, replay); err != nil {
			return err
		}
	}
	return checkpoint.CASPersistentRef(ctx, repo, v1BranchRef, newTip, local) //nolint:wrapcheck // the caller logs it as the reason
}

// checkRemovalPairIsTheDivergence requires the remote tip the removal was
// pushed on (pushed's parent) to be the local removal's parent or one of its
// ancestors, and the local removal to still be in local v1.
func checkRemovalPairIsTheDivergence(ctx context.Context, repo *git.Repository, repoPath string, local, localRemoval, pushed plumbing.Hash) error {
	pushedCommit, err := repo.CommitObject(pushed)
	if err != nil || len(pushedCommit.ParentHashes) != 1 {
		return errNotThisDeletesDivergence
	}
	removalCommit, err := repo.CommitObject(localRemoval)
	if err != nil || len(removalCommit.ParentHashes) != 1 {
		return errNotThisDeletesDivergence
	}
	if !isAncestorOrSelf(ctx, repoPath, pushedCommit.ParentHashes[0], removalCommit.ParentHashes[0]) ||
		!isAncestorOrSelf(ctx, repoPath, localRemoval, local) {
		return errNotThisDeletesDivergence
	}
	return nil
}

func isAncestorOrSelf(ctx context.Context, repoPath string, ancestor, descendant plumbing.Hash) bool {
	if ancestor.Equal(descendant) {
		return true
	}
	base, err := getMergeBase(ctx, repoPath, ancestor.String(), descendant.String())
	return err == nil && base.Equal(ancestor)
}

// replayOnto cherry-picks replay onto base and drops the checkpoint's subtree
// again should a replayed commit have written under its ID.
func replayOnto(ctx context.Context, repo *git.Repository, repoPath string, cid id.CheckpointID, base plumbing.Hash, replay []*object.Commit) (plumbing.Hash, error) {
	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	newTip, err := cherryPickOnto(ctx, repo, base, replay, shallow)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if again, err := buildCheckpointRemovalCommit(ctx, repo, newTip, cid); err == nil {
		return again, nil
	} else if !errors.Is(err, errNothingToRemove) {
		return plumbing.ZeroHash, err
	}
	return newTip, nil
}

func checkpointRemovalMessage(cid id.CheckpointID) string {
	return "Delete checkpoint " + cid.String()
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
	// A queue entry left behind is harmless: pre-push drops entries whose
	// local ref is gone (partitionLocalRefs). So a queue failure is logged and
	// the delete carries on to the remotes instead of stopping half done.
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint delete: could not open the push queue",
			slog.String("checkpoint_id", plan.CheckpointID.String()), slog.String("error", err.Error()))
		return deleted, nil
	}
	refs := []plumbing.ReferenceName{}
	if plan.LocalRef != "" {
		refs = append(refs, plan.LocalRef)
	}
	if canonical, nameErr := checkpoint.RefName(plan.CheckpointID); nameErr == nil && canonical != plan.LocalRef {
		refs = append(refs, canonical)
	}
	if err := queue.Remove(refs); err != nil {
		logging.Warn(ctx, "checkpoint delete: could not remove the checkpoint from the push queue",
			slog.String("checkpoint_id", plan.CheckpointID.String()), slog.String("error", err.Error()))
	}
	return deleted, nil
}

// removeLocalV1Checkpoint commits the subtree's removal onto the local v1
// branch under the checkpoint writers' lock, rebuilding on a CAS conflict. It
// returns the removal commit, and whether there was anything to remove.
func removeLocalV1Checkpoint(ctx context.Context, repo *git.Repository, cid id.CheckpointID) (plumbing.Hash, bool, error) {
	var removal plumbing.Hash
	err := checkpoint.UpdatePersistentRef(ctx, repo, v1BranchRef, func() (plumbing.Hash, plumbing.Hash, error) {
		tip := refTip(repo, v1BranchRef)
		if tip.IsZero() {
			return plumbing.ZeroHash, plumbing.ZeroHash, errNothingToRemove
		}
		commit, err := buildCheckpointRemovalCommit(ctx, repo, tip, cid)
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		removal = commit
		return commit, tip, nil
	})
	switch {
	case errors.Is(err, errNothingToRemove):
		return plumbing.ZeroHash, false, nil
	case err != nil:
		return plumbing.ZeroHash, false, err //nolint:wrapcheck // caller adds the branch context
	}
	return removal, true, nil
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
	commit, err := checkpoint.CreateCommit(ctx, repo, newTree, parent, checkpointRemovalMessage(cid), name, email)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit checkpoint removal: %w", err)
	}
	return commit, nil
}

// deleteFromTarget deletes the per-checkpoint ref and the v1 copy on one
// target, also returning the v1 removal commit it pushed (zero when none).
func deleteFromTarget(ctx context.Context, root string, cid id.CheckpointID, target CheckpointDeleteTarget) (CheckpointDeleteTargetResult, plumbing.Hash) {
	res := CheckpointDeleteTargetResult{Target: target, Ref: DeleteOutcomeAbsent, V1: DeleteOutcomeAbsent}
	var errs []string
	if !target.RefOID.IsZero() {
		outcome, reason := deleteRemoteRef(ctx, root, target.URL, cid, target.RefName, target.RefOID)
		res.Ref = outcome
		if reason != "" {
			errs = append(errs, reason)
		}
	}
	var pushedV1 plumbing.Hash
	if !target.V1Tip.IsZero() && target.V1 != V1CopyAbsent {
		outcome, reason, pushed := deleteFromRemoteV1(ctx, root, cid, target)
		res.V1, pushedV1 = outcome, pushed
		if reason != "" {
			errs = append(errs, reason)
		}
	}
	res.Error = strings.Join(errs, "; ")
	return res, pushedV1
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
func deleteFromRemoteV1(ctx context.Context, root string, cid id.CheckpointID, target CheckpointDeleteTarget) (DeleteOutcome, string, plumbing.Hash) {
	tmpRef := plumbing.ReferenceName(FetchTmpRefPrefix + "checkpoint-delete/" + cid.String())
	if err := fetchURLIntoTmpRef(ctx, root, target.URL, v1BranchRef.String(), tmpRef.String(), "checkpoint branch", true, checkpointRemoteForegroundFetchTimeout); err != nil {
		return DeleteOutcomeFailed, err.Error(), plumbing.ZeroHash
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return DeleteOutcomeFailed, err.Error(), plumbing.ZeroHash
	}
	defer repo.Close()
	tip := refTip(repo, tmpRef)
	defer func() {
		if !tip.IsZero() {
			_ = gitrepo.CompareAndSwapRef(ctx, root, tmpRef, plumbing.ZeroHash, tip) //nolint:errcheck // temp ref cleanup is best-effort
		}
	}()
	if tip.IsZero() {
		return DeleteOutcomeFailed, "fetched checkpoint branch is missing", plumbing.ZeroHash
	}

	commit, err := buildCheckpointRemovalCommit(ctx, repo, tip, cid)
	if errors.Is(err, errNothingToRemove) {
		// Already gone there (for example deleted through another spelling
		// of the same URL): the tracking ref still has to stop showing it.
		advanceTrackingV1(ctx, root, repo, target, tip)
		return DeleteOutcomeAbsent, "", plumbing.ZeroHash
	}
	if err != nil {
		return DeleteOutcomeFailed, err.Error(), plumbing.ZeroHash
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
		return outcome, reason, plumbing.ZeroHash
	}
	advanceTrackingV1(ctx, root, repo, target, commit)
	return DeleteOutcomeDeleted, "", commit
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
