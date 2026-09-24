package strategy

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

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

// stampedTrailer returns the first checkpoint trailer in message that
// prepare-commit-msg stamped itself, skipping trailers inherited from
// squashed commits: those link existing checkpoints and never stand in for a
// fresh one.
func stampedTrailer(message string, inherited []id.CheckpointID) (id.CheckpointID, bool) {
	skip := make(map[id.CheckpointID]bool, len(inherited))
	for _, cpID := range inherited {
		skip[cpID] = true
	}
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		if !skip[cpID] {
			return cpID, true
		}
	}
	return id.EmptyCheckpointID, false
}

// pickCondensationTarget chooses the trailer a commit's new work condenses
// into. A lone trailer is it. Among several, trailers that already have a
// checkpoint are links; the stamped one is the last without a checkpoint,
// since prepare appends after the inherited ones. None means the commit only
// links existing work.
func pickCondensationTarget(ids []id.CheckpointID, exists func(id.CheckpointID) bool) (id.CheckpointID, bool) {
	if len(ids) == 1 {
		return ids[0], true
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if !exists(ids[i]) {
			return ids[i], true
		}
	}
	return id.EmptyCheckpointID, false
}

func pickCondensationTargetState(ids []id.CheckpointID, exists func(id.CheckpointID) bool) (target id.CheckpointID, preexisting, found bool) {
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false, false
	}
	if len(ids) == 1 {
		return ids[0], exists(ids[0]), true
	}
	target, found = pickCondensationTarget(ids, exists)
	if !found {
		return id.EmptyCheckpointID, false, false
	}
	// Recheck after selection: another session may have created the checkpoint
	// between the first store read and condensation.
	return target, exists(target), true
}

// condensationTarget picks the trailer this commit condenses into and reports
// whether that checkpoint already existed before this post-commit began. Read
// once per commit: sessions condensing into the same fresh checkpoint later in
// this hook must not look like writes into someone else's.
func (s *ManualCommitStrategy) condensationTarget(ctx context.Context, repo *git.Repository, ids []id.CheckpointID) (target id.CheckpointID, preexisting, found bool) {
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false, false
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		// Nothing can be told apart without a store; the last trailer is
		// where prepare stamps.
		return ids[len(ids)-1], false, true
	}
	exists := func(cpID id.CheckpointID) bool { return checkpointExists(ctx, store, cpID) }
	target, preexisting, ok := pickCondensationTargetState(ids, exists)
	if !ok {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "post-commit: every trailer links an existing checkpoint; nothing to condense",
			slog.Int("trailers", len(ids)))
	}
	return target, preexisting, ok
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
const inheritedTrailersFile = "entire-inherited-trailers.json"

type inheritedTrailers struct {
	Parent string            `json:"parent"`
	IDs    []id.CheckpointID `json:"ids"`
}

// recordInheritedTrailers tells post-commit which trailers this commit
// inherited, so it never condenses into one: an inherited trailer links an
// existing checkpoint, which may simply not be in this clone's store yet.
// With nothing inherited it clears any marker a commit that never happened left.
func recordInheritedTrailers(ctx context.Context, inherited []id.CheckpointID) {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return
	}
	if len(inherited) == 0 {
		_ = osroot.RemoveNoSymlinks(root, inheritedTrailersFile) //nolint:errcheck // absent is the usual case
		return
	}
	marker := inheritedTrailers{IDs: inherited, Parent: headHash(ctx)}
	writeInheritedTrailers(ctx, root, marker)
}

// recordInheritedTrailersOnAmend is recordInheritedTrailers for an amend, whose
// commit replaces HEAD and so has HEAD's parent as its own.
func recordInheritedTrailersOnAmend(ctx context.Context, inherited []id.CheckpointID) {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil || len(inherited) == 0 {
		return
	}
	writeInheritedTrailers(ctx, root, inheritedTrailers{IDs: inherited, Parent: headParentHash(ctx)})
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

// headParentHash returns HEAD's first parent, the parent an amend will have.
func headParentHash(ctx context.Context) string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return ""
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil || len(commit.ParentHashes) == 0 {
		return ""
	}
	return commit.ParentHashes[0].String()
}

// headHash returns HEAD's commit, the parent the commit being prepared will
// have; empty before the first commit.
func headHash(ctx context.Context) string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return ""
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	return head.Hash().String()
}

// stampedTrailersOf returns the checkpoint trailers of commit that
// prepare-commit-msg stamped, leaving out the ones it recorded as inherited.
func stampedTrailersOf(ctx context.Context, commit *object.Commit) []id.CheckpointID {
	var parent string
	if len(commit.ParentHashes) > 0 {
		parent = commit.ParentHashes[0].String()
	}
	return withoutInherited(trailers.ParseAllCheckpoints(commit.Message), takeInheritedTrailers(ctx, parent))
}

// takeInheritedTrailers returns the trailers prepare-commit-msg recorded as
// inherited for a commit on parent, and consumes the marker either way.
func takeInheritedTrailers(ctx context.Context, parent string) map[id.CheckpointID]bool {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return nil
	}
	data, err := osroot.ReadFileNoFollow(root, inheritedTrailersFile)
	if err != nil {
		return nil
	}
	_ = osroot.RemoveNoSymlinks(root, inheritedTrailersFile) //nolint:errcheck // a stale marker is ignored by its parent check
	var marker inheritedTrailers
	if json.Unmarshal(data, &marker) != nil || marker.Parent != parent {
		return nil
	}
	out := make(map[id.CheckpointID]bool, len(marker.IDs))
	for _, cpID := range marker.IDs {
		out[cpID] = true
	}
	return out
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
