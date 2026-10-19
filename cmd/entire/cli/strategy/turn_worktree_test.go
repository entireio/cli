package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir — do NOT add t.Parallel().

func TestSplitEditsByWorktree(t *testing.T) {
	mainDir := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	nested := filepath.Join(mainDir, ".worktrees", "nested")
	testutil.RunGit(t, mainDir, "worktree", "add", linked, "-b", "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", nested, "-b", "nested")
	chdirWorktree(t, mainDir)

	got := SplitEditsByWorktree(t.Context(), mainDir, []string{
		filepath.Join(linked, "a.go"),
		filepath.Join(linked, "pkg", "b.go"),
		filepath.Join(nested, "n.go"),
		filepath.Join(mainDir, "home.go"), // the hook's own tree records itself
		"rel.go",                          // relative: the hook's tree
		"/tmp/plan.md",                    // outside every worktree
		filepath.Join(linked, ".git"),     // no commit carries it
		filepath.Join(linked, ".entire", "x"),
	})
	byResolved := map[string][]string{}
	for root, rels := range got {
		byResolved[mustEvalSymlinks(t, root)] = rels
	}
	assert.Equal(t, map[string][]string{
		mustEvalSymlinks(t, linked): {"a.go", "pkg/b.go"},
		mustEvalSymlinks(t, nested): {"n.go"},
	}, byResolved, "a nested worktree owns its files; the hook's tree and outside paths are left out")

	assert.Nil(t, SplitEditsByWorktree(t.Context(), mainDir, []string{filepath.Join(mainDir, "home.go")}))
}

// A path reached through a symlink is placed in the worktree it is in.
func TestSplitEditsByWorktree_ResolvesSymlinkedPaths(t *testing.T) {
	mainDir := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", linked, "-b", "linked")
	chdirWorktree(t, mainDir)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(linked, alias))

	got := SplitEditsByWorktree(t.Context(), mainDir, []string{filepath.Join(alias, "a.go")})
	require.Len(t, got, 1)
	for root, rels := range got {
		assert.Equal(t, mustEvalSymlinks(t, linked), mustEvalSymlinks(t, root))
		assert.Equal(t, []string{"a.go"}, rels)
	}
}

// A step for another worktree records into that worktree's entry: its files
// hashed there and its HEAD as the entry's base, the home untouched.
func TestSaveStep_InAnotherWorktreeRecordsItsEntry(t *testing.T) {
	s, _, linkedDir := homedSession(t, "step-elsewhere")
	before := mustLoad(t, s, "step-elsewhere")
	testutil.WriteFile(t, linkedDir, "w.go", "package w")
	require.NoError(t, s.SaveStep(t.Context(), StepContext{
		SessionID:     "step-elsewhere",
		WorktreeRoot:  linkedDir,
		ModifiedFiles: []string{"w.go"},
		SameTurn:      true,
		MetadataDir:   paths.SessionMetadataDirFromSessionID("step-elsewhere"),
		CommitMessage: "Checkpoint",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))
	state := mustLoad(t, s, "step-elsewhere")
	assert.Equal(t, before.FilesTouched, state.FilesTouched)
	assert.Equal(t, before.BaseCommit, state.BaseCommit)
	assert.Equal(t, before.StepCount, state.StepCount, "a further step of the same turn isn't counted again")
	work := state.OtherWorktrees[linkedKey(t, linkedDir)]
	require.NotNil(t, work)
	assert.Equal(t, []string{"w.go"}, work.FilesTouched)
	assert.NotEmpty(t, work.TouchedFileHashes["w.go"])
	assert.Equal(t, testutil.GetHeadHash(t, linkedDir), work.BaseCommit)
}

// Records of a worktree git no longer lists are dropped, so a removed
// worktree can't keep the session pending.
func TestPruneRemovedWorktrees(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "prune")
	require.NoError(t, MutateSessionState(t.Context(), "prune", func(state *SessionState) error {
		state.OtherWorktrees = map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"w.go"}, Incarnation: mark(t, linkedDir)},
			"gone":                  {Path: filepath.Join(t.TempDir(), "gone"), FilesTouched: []string{"g.go"}},
		}
		return nil
	}))
	PruneRemovedWorktrees(t.Context(), "prune", mainDir)
	state := mustLoad(t, s, "prune")
	assert.Contains(t, state.OtherWorktrees, linkedKey(t, linkedDir))
	assert.NotContains(t, state.OtherWorktrees, "gone")

	testutil.RunGit(t, mainDir, "worktree", "remove", "--force", linkedDir)
	PruneRemovedWorktrees(t.Context(), "prune", mainDir)
	assert.Nil(t, mustLoad(t, s, "prune").OtherWorktrees)
}

// A moved worktree keeps its entry (keyed by worktree ID), which takes the new
// path; only worktrees git no longer lists are pruned.
func TestPruneRemovedWorktrees_KeepsMovedWorktrees(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "prune-moved")
	key := linkedKey(t, linkedDir)
	require.NoError(t, MutateSessionState(t.Context(), "prune-moved", func(state *SessionState) error {
		state.OtherWorktrees = map[string]*session.WorktreeWork{key: {Path: mustEvalSymlinks(t, linkedDir), FilesTouched: []string{"w.go"}, Incarnation: mark(t, linkedDir)}}
		return nil
	}))
	moved := filepath.Join(t.TempDir(), "moved")
	testutil.RunGit(t, mainDir, "worktree", "move", linkedDir, moved)

	PruneRemovedWorktrees(t.Context(), "prune-moved", mainDir)
	work := mustLoad(t, s, "prune-moved").OtherWorktrees[key]
	require.NotNil(t, work, "a moved worktree's pending edits were pruned")
	assert.Equal(t, []string{"w.go"}, work.FilesTouched)
	assert.Equal(t, mustEvalSymlinks(t, moved), work.Path)
}
