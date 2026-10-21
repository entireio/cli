package strategy

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir and t.Setenv — do NOT add t.Parallel().

// readdLinked removes the linked worktree and adds a new one at the same path,
// which git gives the same worktree ID.
func readdLinked(t *testing.T, mainDir, linkedDir string) {
	t.Helper()
	before := linkedKey(t, linkedDir)
	testutil.RunGit(t, mainDir, "worktree", "remove", "--force", linkedDir)
	testutil.RunGit(t, mainDir, "worktree", "add", linkedDir, "-b", "readded")
	require.Equal(t, before, linkedKey(t, linkedDir), "git did not reuse the worktree ID")
}

// adminDir is the linked worktree's git admin directory.
func adminDir(t *testing.T, mainDir, linkedDir string) string {
	t.Helper()
	return filepath.Join(mainDir, ".git", "worktrees", linkedKey(t, linkedDir))
}

// A record of edits in a worktree that was since removed and re-added under
// the same git ID belongs to the removed one: it doesn't link the new
// worktree's commits, and the next edit there starts a fresh record.
func TestWorktreeRecord_ReaddedWorktreeIsNotTheRecordedOne(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	key := linkedKey(t, linkedDir)
	state := &SessionState{
		SessionID:    "readd",
		WorktreePath: mainDir,
		FilesTouched: []string{"home.go"},
		OtherWorktrees: map[string]*session.WorktreeWork{
			key: {Path: linkedDir, FilesTouched: []string{"a.go"}, Incarnation: mark(t, linkedDir)},
		},
	}
	require.True(t, hasWorktreeEntry(ctx, state, linkedDir))

	readdLinked(t, mainDir, linkedDir)

	assert.False(t, hasWorktreeEntry(ctx, state, linkedDir), "the removed worktree's record links the new one")
	_, ok := enterWorktreeView(ctx, state, linkedDir)
	assert.False(t, ok, "a commit in the new worktree is judged by the removed one's record")

	leave, ok := enterStepWorktree(ctx, state, linkedDir)
	require.True(t, ok)
	assert.Empty(t, state.FilesTouched, "a step in the new worktree inherited the removed one's files")
	state.FilesTouched = []string{"b.go"}
	leave()
	work := state.OtherWorktrees[key]
	require.NotNil(t, work)
	assert.Equal(t, []string{"b.go"}, work.FilesTouched)
	assert.Equal(t, mark(t, linkedDir), work.Incarnation)
	assert.Equal(t, []string{"home.go"}, state.FilesTouched)
}

// Pruning drops a record of a worktree git still lists under the same ID, but
// for a worktree added since.
func TestPruneRemovedWorktrees_DropsReaddedWorktree(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "prune-readd")
	key := linkedKey(t, linkedDir)
	require.NoError(t, MutateSessionState(t.Context(), "prune-readd", func(state *SessionState) error {
		state.OtherWorktrees = map[string]*session.WorktreeWork{key: {Path: linkedDir, FilesTouched: []string{"w.go"}, Incarnation: mark(t, linkedDir)}}
		return nil
	}))
	PruneRemovedWorktrees(t.Context(), "prune-readd", mainDir)
	require.Contains(t, mustLoad(t, s, "prune-readd").OtherWorktrees, key)

	readdLinked(t, mainDir, linkedDir)
	PruneRemovedWorktrees(t.Context(), "prune-readd", mainDir)
	assert.Nil(t, mustLoad(t, s, "prune-readd").OtherWorktrees)
}

// An entry another hook records while pruning lists the worktrees — for a
// worktree that listing predates — survives: the listing is taken under the
// session lock, so that hook waits for it.
func TestPruneRemovedWorktrees_KeepsEntryRecordedWhileListing(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "prune-race")
	key := linkedKey(t, linkedDir)
	require.NoError(t, MutateSessionState(t.Context(), "prune-race", func(state *SessionState) error {
		state.OtherWorktrees = map[string]*session.WorktreeWork{key: {Path: linkedDir, FilesTouched: []string{"w.go"}, Incarnation: mark(t, linkedDir)}}
		return nil
	}))
	added := filepath.Join(t.TempDir(), "added")
	var recorded chan error
	list := func(ctx context.Context, root string) ([]string, error) {
		listed, err := gitrepo.ListWorktreePaths(ctx, root)
		// Another hook adds a worktree and records an edit there.
		testutil.RunGit(t, mainDir, "worktree", "add", added, "-b", "added")
		addedMarker, addedKey := mark(t, added), linkedKey(t, added)
		recorded = make(chan error, 1)
		go func() {
			recorded <- MutateSessionState(context.Background(), "prune-race", func(state *SessionState) error {
				state.OtherWorktrees[addedKey] = &session.WorktreeWork{Path: added, FilesTouched: []string{"n.go"}, Incarnation: addedMarker}
				return nil
			})
		}()
		select {
		case err := <-recorded:
			recorded <- err // not held off: the listing isn't under the lock
		case <-time.After(300 * time.Millisecond):
		}
		return listed, err
	}
	pruneRemovedWorktrees(t.Context(), "prune-race", mainDir, list)
	require.NotNil(t, recorded, "prune never listed the worktrees")
	require.NoError(t, <-recorded)

	others := mustLoad(t, s, "prune-race").OtherWorktrees
	assert.Contains(t, others, key)
	assert.Contains(t, others, linkedKey(t, added), "prune deleted an entry recorded after its listing")
}

// A marker that isn't one Entire wrote — malformed, or a symlink — doesn't
// vouch for a record.
func TestWorktreeIncarnation_RefusesForeignMarkers(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	key := linkedKey(t, linkedDir)
	marker := mark(t, linkedDir)
	work := &session.WorktreeWork{Path: linkedDir, FilesTouched: []string{"a.go"}, Incarnation: marker}
	require.True(t, worktreeEntryLinks(ctx, linkedDir, key, work))
	assert.True(t, worktreeEntryLinks(ctx, mainDir, session.WorktreeKey(""), &session.WorktreeWork{}), "the main worktree has no marker to check")
	assert.False(t, worktreeEntryLinks(ctx, linkedDir, key, &session.WorktreeWork{FilesTouched: []string{"a.go"}}), "an unmarked record counts")

	file := filepath.Join(adminDir(t, mainDir, linkedDir), worktreeIncarnationFile)
	require.NoError(t, os.WriteFile(file, []byte("not a marker"), 0o600))
	assert.False(t, worktreeEntryLinks(ctx, linkedDir, key, work), "a malformed marker vouched for a record")
	_, err := MarkWorktree(ctx, linkedDir, key)
	require.Error(t, err, "a malformed marker was accepted")

	elsewhere := filepath.Join(t.TempDir(), "marker")
	require.NoError(t, os.WriteFile(elsewhere, []byte(marker), 0o600))
	require.NoError(t, os.Remove(file))
	require.NoError(t, os.Symlink(elsewhere, file))
	assert.False(t, worktreeEntryLinks(ctx, linkedDir, key, work), "a symlinked marker vouched for a record")
}

// Hooks marking the same worktree at once agree on one marker.
func TestWorktreeIncarnation_ConcurrentCreatesAgree(t *testing.T) {
	_, linkedDir := twoWorktrees(t)
	worktreeID, err := paths.GetWorktreeID(linkedDir)
	require.NoError(t, err)
	const n = 8
	markers := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { markers[i], errs[i] = MarkWorktree(context.Background(), linkedDir, worktreeID) })
	}
	wg.Wait()
	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, markers[0], markers[i])
	}
	assert.Len(t, markers[0], 32)
}

// breakMarkerChecks makes every marker check for the worktree at root fail
// without saying anything about the marker, as a git or filesystem failure
// would, until the test ends.
func breakMarkerChecks(t *testing.T, root string) {
	t.Helper()
	notADir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))
	for _, r := range []string{root, resolveExisting(root)} {
		worktreeCommonDirs.Store(r, notADir)
		t.Cleanup(func() { worktreeCommonDirs.Delete(r) })
	}
}

// A check that fails says nothing about a record: it doesn't link, but
// neither pruning nor a turn in that worktree drops it.
func TestWorktreeRecord_FailedCheckKeepsRecord(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "check-fails")
	key := linkedKey(t, linkedDir)
	marker := mark(t, linkedDir)
	require.NoError(t, MutateSessionState(t.Context(), "check-fails", func(state *SessionState) error {
		state.OtherWorktrees = map[string]*session.WorktreeWork{key: {Path: linkedDir, FilesTouched: []string{"w.go"}, Incarnation: marker}}
		return nil
	}))
	breakMarkerChecks(t, linkedDir)

	state := mustLoad(t, s, "check-fails")
	current, err := worktreeEntryCurrent(t.Context(), linkedDir, key, state.OtherWorktrees[key])
	require.Error(t, err)
	assert.False(t, current)
	assert.False(t, hasWorktreeEntry(t.Context(), state, linkedDir), "an unverified record links")

	PruneRemovedWorktrees(t.Context(), "check-fails", mainDir)
	require.Contains(t, mustLoad(t, s, "check-fails").OtherWorktrees, key, "prune dropped a record it couldn't check")

	state = mustLoad(t, s, "check-fails")
	_, ok := enterStepWorktree(t.Context(), state, linkedDir)
	assert.False(t, ok)
	work := state.OtherWorktrees[key]
	require.NotNil(t, work, "a turn dropped a record it couldn't check")
	assert.Equal(t, []string{"w.go"}, work.FilesTouched)
	assert.Equal(t, marker, work.Incarnation)
}
