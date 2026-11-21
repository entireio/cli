package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// stampedTrailer returns the checkpoint trailer in message's final trailer
// block that prepare-commit-msg stamped itself (recordStampedTrailer), the last
// one, as post-commit picks it. Any other ID there (pasted, reused with
// `commit -C`, inherited from squashed commits) names a checkpoint this commit
// did not stamp, and would otherwise suppress the fresh trailer new work
// needs; so does an ID anywhere else in the message. A checkpoint deleted from
// this clone never counts either. rules is messageCommentRules' answer for
// message.
func stampedTrailer(ctx context.Context, message string, rules commentRules, inherited []id.CheckpointID) (id.CheckpointID, bool) {
	ids := withoutInherited(withoutDeletedCheckpoints(ctx, preparedFinalTrailers(message, rules)), checkpointSet(inherited))
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false
	}
	return lastStamped(ids, readStampedTrailers(ctx))
}

// preparedFinalTrailers returns the checkpoint IDs in the final trailer block
// of a message prepare-commit-msg was handed, as post-commit will read the
// recorded commit: cleaned as git will clean it (an edited message still
// carries git's comment block and, with `commit -v`, a scissors line and diff
// below the message), then read by git's trailer grammar.
func preparedFinalTrailers(message string, rules commentRules) []id.CheckpointID {
	message = trailers.CutAtScissors(message, rules.prefix)
	if rules.stripped {
		message = trailers.StripEditorComments(message, rules.prefix)
	}
	return trailers.ParseAllCheckpointsFromFinalTrailerBlock(message, rules.trailerPrefix)
}

// lastStamped returns the last of ids that stamped holds.
func lastStamped(ids []id.CheckpointID, stamped map[id.CheckpointID]bool) (id.CheckpointID, bool) {
	for i := len(ids) - 1; i >= 0; i-- {
		if stamped[ids[i]] {
			return ids[i], true
		}
	}
	return id.EmptyCheckpointID, false
}

// pickCondensationTarget chooses the trailer a commit's new work condenses
// into, and reports whether its checkpoint already exists. It is the last
// trailer prepare-commit-msg stamped for this commit. Without one (an amend
// keeps the amended commit's trailer; a sequence operation runs no prepare)
// only an existing checkpoint can take the work, and only from the session
// that owns it (stampedByAnotherCommit): the last trailer whose checkpoint
// exists. A checkpoint this clone does not have is created only for a trailer
// prepare stamped, never for one the message brought (pasted, reused with
// `commit -C`, typed in the editor, a teammate's not yet fetched).
func pickCondensationTarget(ids []id.CheckpointID, stamped map[id.CheckpointID]bool, exists func(id.CheckpointID) bool) (target id.CheckpointID, preexisting, found bool) {
	if target, ok := lastStamped(ids, stamped); ok {
		return target, exists(target), true
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if exists(ids[i]) {
			return ids[i], true, true
		}
	}
	return id.EmptyCheckpointID, false, false
}

// condensationTarget picks the trailer this commit condenses into among ids,
// given the ones prepare stamped, and reports whether that checkpoint already
// existed before this post-commit began. Read once per commit: sessions
// condensing into the same fresh checkpoint later in this hook must not look
// like writes into someone else's.
func (s *ManualCommitStrategy) condensationTarget(ctx context.Context, repo *git.Repository, ids []id.CheckpointID, stamped map[id.CheckpointID]bool) (target id.CheckpointID, preexisting, found bool) {
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false, false
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		// Nothing can be told apart without a store; only a trailer prepare
		// stamped may be written, as a new checkpoint.
		target, found = lastStamped(ids, stamped)
		return target, false, found
	}
	return pickCondensationTarget(ids, stamped, func(cpID id.CheckpointID) bool { return checkpointExists(ctx, store, cpID) })
}

type preexistingTargetKey struct{}

// withPreexistingTarget marks a post-commit whose condensation target already
// had a checkpoint when the hook began.
func withPreexistingTarget(ctx context.Context) context.Context {
	return context.WithValue(ctx, preexistingTargetKey{}, true)
}

// stampedByAnotherCommit reports whether checkpointID is a preexisting
// checkpoint this session did not stamp for this commit: neither the one it is
// amending (LastCheckpointID) nor its pending reservation. Writing there would
// fold new work into another commit's record.
func stampedByAnotherCommit(ctx context.Context, checkpointID id.CheckpointID, state *SessionState) bool {
	if ctx.Value(preexistingTargetKey{}) != true {
		return false
	}
	return checkpointID != state.LastCheckpointID && checkpointID != state.PendingCondensationID()
}

func checkpointExists(ctx context.Context, store checkpoint.PersistentStore, checkpointID id.CheckpointID) bool {
	summary, err := store.Read(ctx, checkpointID)
	return err == nil && summary != nil
}

// inheritedTrailersFile carries the trailers prepare-commit-msg inherited from
// squashed commits to post-commit. It lives in the per-worktree git dir, like
// SQUASH_MSG, and is tied to the commit's parent so a commit that never
// happened cannot speak for the next one.
//
// Both possible parents are recorded: HEAD for an ordinary commit and HEAD's
// parent for an amend. git's source word cannot tell them apart (`--amend -m`
// reports "message", `-C <rev>` reports "commit"), and post-commit sees the
// real parent.
const inheritedTrailersFile = "entire-inherited-trailers.json"

type inheritedTrailers struct {
	Parents []string          `json:"parents"`
	IDs     []id.CheckpointID `json:"ids"`
	// Redone are the committed paths a redo's inherited checkpoints already
	// hold; post-commit does not count them as a session's overlap evidence.
	Redone []string `json:"redone,omitempty"`
}

// recordInheritedTrailers tells post-commit which trailers this commit
// inherited, so it never condenses into one: an inherited trailer links an
// existing checkpoint, which may simply not be in this clone's store yet.
// With nothing inherited it clears any marker a commit that never happened left.
func recordInheritedTrailers(ctx context.Context, inherited []id.CheckpointID) {
	recordInheritedMarker(ctx, inherited, nil)
}

// recordInheritedMarker is recordInheritedTrailers that also records the
// paths a redo's inherited checkpoints hold.
func recordInheritedMarker(ctx context.Context, inherited []id.CheckpointID, redone map[string]bool) {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return
	}
	if len(inherited) == 0 {
		_ = osroot.RemoveNoSymlinks(root, inheritedTrailersFile) //nolint:errcheck // absent is the usual case
		return
	}
	writeInheritedTrailers(ctx, root, inheritedTrailers{
		IDs:     inherited,
		Parents: commitParentCandidates(ctx),
		Redone:  slices.Sorted(maps.Keys(redone)),
	})
}

func writeInheritedTrailers(ctx context.Context, root *os.Root, marker inheritedTrailers) {
	data, err := json.Marshal(marker)
	if err != nil {
		return
	}
	if err := jsonutil.WriteFileAtomicIn(root, inheritedTrailersFile, data, 0o600); err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: could not record inherited trailers",
			slog.String("error", err.Error()))
	}
}

// commitParentCandidates returns the parents the commit being prepared can
// have: HEAD, and HEAD's first parent if it is an amend. Empty before the
// first commit.
func commitParentCandidates(ctx context.Context) []string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return nil
	}
	out := []string{head.Hash().String()}
	if commit, err := repo.CommitObject(head.Hash()); err == nil && len(commit.ParentHashes) > 0 {
		out = append(out, commit.ParentHashes[0].String())
	}
	return out
}

// linkedTrailersOf returns the checkpoint trailers commit links
// (LinkedCheckpoints), plus any trailer prepare-commit-msg stamped that a
// paragraph added below it (an editor, a commit-msg hook) moved out of the
// final trailer block, leaving out the ones prepare recorded as inherited and any
// checkpoint deleted from this clone: post-commit must never condense new work
// into a deleted ID, which would re-create it. It also returns, consuming the
// record, the IDs prepare stamped (see pickCondensationTarget), the committed
// paths a redo's inherited checkpoints hold, and the stamped trailers found
// outside the final trailer block.
func linkedTrailersOf(ctx context.Context, commit *object.Commit) (ids []id.CheckpointID, stamped map[id.CheckpointID]bool, redone map[string]bool, moved []id.CheckpointID) {
	var parent string
	if len(commit.ParentHashes) > 0 {
		parent = commit.ParentHashes[0].String()
	}
	var inherited map[id.CheckpointID]bool
	inherited, redone = takeInheritedMarker(ctx, parent)
	stamped = takeStampedTrailers(ctx)
	linked := LinkedCheckpoints(ctx, commit.Message)
	moved = stampedOutsideFinalBlock(linked, commit.Message, stamped)
	ids = withoutInherited(append(slices.Clone(moved), linked...), inherited)
	return withoutDeletedCheckpoints(ctx, ids), stamped, redone, moved
}

// stampedOutsideFinalBlock returns the stamped trailers message names outside
// its final trailer block (linked). The stamp record, not the trailer's
// position, proves the ID is this commit's. Callers put them before linked, so
// a stamped trailer in the final trailer block stays the condensation target.
func stampedOutsideFinalBlock(linked []id.CheckpointID, message string, stamped map[id.CheckpointID]bool) []id.CheckpointID {
	var moved []id.CheckpointID
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		if stamped[cpID] && !slices.Contains(linked, cpID) {
			moved = append(moved, cpID)
		}
	}
	return moved
}

type recordedCommitLinkKey struct{}

// withRecordedCommitLink marks a post-commit whose condensation target is a
// stamped trailer outside the commit's final trailer block. Commands that
// write into a commit's checkpoint read only that block, so the checkpoint
// records the commit (LinkedCommits) for them to find it by, as attach does
// for a pushed commit it links without a trailer.
func withRecordedCommitLink(ctx context.Context, commitSHA string) context.Context {
	return context.WithValue(ctx, recordedCommitLinkKey{}, commitSHA)
}

// recordedCommitLink returns the commit withRecordedCommitLink marked.
func recordedCommitLink(ctx context.Context) (string, bool) {
	sha, ok := ctx.Value(recordedCommitLinkKey{}).(string)
	return sha, ok && sha != ""
}

// stampedTrailersFile lists the trailers prepare-commit-msg stamped since the
// last post-commit: IDs it generated or took from a session's reservation. An
// amend's restored trailer names an existing checkpoint and is not listed.
// Post-commit may create the checkpoint of only these (pickCondensationTarget).
// Per worktree, like inheritedTrailersFile. An entry a commit that never
// happened left stays until the next post-commit; it names a checkpoint only
// this clone minted and nothing holds, so no commit can steal it.
const stampedTrailersFile = "entire-stamped-trailers.json"

// recordStampedTrailer adds cpID to the trailers prepare-commit-msg stamped,
// and reports whether it did. Prepare writes a trailer only once its stamp is
// recorded: post-commit would never create the checkpoint of an unrecorded
// one, so the commit would name a checkpoint that does not exist.
func recordStampedTrailer(ctx context.Context, cpID id.CheckpointID) bool {
	err := writeStampedTrailer(ctx, cpID)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: could not record the stamped trailer, leaving the commit unlinked",
			slog.String("checkpoint_id", cpID.String()),
			slog.String("error", err.Error()))
	}
	return err == nil
}

func writeStampedTrailer(ctx context.Context, cpID id.CheckpointID) error {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return err
	}
	stamped := readStampedTrailersIn(root)
	if slices.Contains(stamped, cpID) {
		return nil
	}
	data, err := json.Marshal(append(stamped, cpID))
	if err != nil {
		return fmt.Errorf("encode stamped trailers: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, stampedTrailersFile, data, 0o600) //nolint:wrapcheck // recordStampedTrailer logs it with context
}

// readStampedTrailers returns the trailers prepare-commit-msg stamped since the
// last post-commit.
func readStampedTrailers(ctx context.Context) map[id.CheckpointID]bool {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return nil
	}
	return checkpointSet(readStampedTrailersIn(root))
}

// takeStampedTrailers is readStampedTrailers for post-commit, which consumes
// the record.
func takeStampedTrailers(ctx context.Context) map[id.CheckpointID]bool {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return nil
	}
	stamped := readStampedTrailersIn(root)
	_ = osroot.RemoveNoSymlinks(root, stampedTrailersFile) //nolint:errcheck // absent when prepare stamped nothing
	return checkpointSet(stamped)
}

func readStampedTrailersIn(root *os.Root) []id.CheckpointID {
	data, err := osroot.ReadFileNoFollow(root, stampedTrailersFile)
	if err != nil {
		return nil
	}
	var stamped []id.CheckpointID
	if json.Unmarshal(data, &stamped) != nil {
		return nil
	}
	return stamped
}

func checkpointSet(ids []id.CheckpointID) map[id.CheckpointID]bool {
	set := make(map[id.CheckpointID]bool, len(ids))
	for _, cpID := range ids {
		set[cpID] = true
	}
	return set
}

// takeInheritedTrailers returns the trailers prepare-commit-msg recorded as
// inherited for a commit on parent, and consumes the marker either way.
func takeInheritedTrailers(ctx context.Context, parent string) map[id.CheckpointID]bool {
	inherited, _ := takeInheritedMarker(ctx, parent)
	return inherited
}

// takeInheritedMarker is takeInheritedTrailers that also returns the recorded
// redone paths.
func takeInheritedMarker(ctx context.Context, parent string) (map[id.CheckpointID]bool, map[string]bool) {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return nil, nil
	}
	data, err := osroot.ReadFileNoFollow(root, inheritedTrailersFile)
	if err != nil {
		return nil, nil
	}
	_ = osroot.RemoveNoSymlinks(root, inheritedTrailersFile) //nolint:errcheck // a stale marker is ignored by its parent check
	var marker inheritedTrailers
	if json.Unmarshal(data, &marker) != nil || !slices.Contains(marker.Parents, parent) {
		return nil, nil
	}
	out := checkpointSet(marker.IDs)
	var redone map[string]bool
	for _, p := range marker.Redone {
		if redone == nil {
			redone = make(map[string]bool, len(marker.Redone))
		}
		redone[p] = true
	}
	return out, redone
}

// withoutInherited drops the trailers a commit inherited: post-commit condenses
// only into what prepare stamped.
func withoutInherited(ids []id.CheckpointID, inherited map[id.CheckpointID]bool) []id.CheckpointID {
	if len(inherited) == 0 {
		return ids
	}
	out := make([]id.CheckpointID, 0, len(ids))
	for _, cpID := range ids {
		if !inherited[cpID] {
			out = append(out, cpID)
		}
	}
	return out
}

// perWorktreeGitRoot opens the per-worktree git dir, where SQUASH_MSG and the
// sequencer markers live. The root is the shared registry handle: never close it.
func perWorktreeGitRoot(ctx context.Context) (*os.Root, error) {
	gitDir, err := GetGitDir(ctx)
	if err != nil {
		return nil, err
	}
	return gitdir.OpenAt(gitDir) //nolint:wrapcheck // callers treat any error as "no marker"
}
