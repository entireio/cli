package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir — do NOT add t.Parallel().

func TestTurnWorktree(t *testing.T) {
	mainDir := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	nested := filepath.Join(mainDir, ".worktrees", "nested")
	testutil.RunGit(t, mainDir, "worktree", "add", linked, "-b", "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", nested, "-b", "nested")
	chdirWorktree(t, mainDir)
	main := mustEvalSymlinks(t, mainDir)

	cases := map[string]struct {
		edited []string
		want   string
	}{
		"all in the linked worktree":          {[]string{filepath.Join(linked, "a.go"), filepath.Join(linked, "pkg", "b.go")}, mustEvalSymlinks(t, linked)},
		"nested worktree wins over its host":  {[]string{filepath.Join(nested, "n.go")}, mustEvalSymlinks(t, nested)},
		"relative paths are the hook's tree":  {[]string{"rel.go"}, main},
		"edits outside every worktree":        {[]string{filepath.Join(linked, "a.go"), "/tmp/plan.md", filepath.Join(t.TempDir(), "x")}, mustEvalSymlinks(t, linked)},
		"two worktrees":                       {[]string{filepath.Join(linked, "a.go"), filepath.Join(mainDir, "m.go")}, ""},
		"nothing in any worktree":             {[]string{"/tmp/plan.md"}, ""},
		"paths no commit carries don't count": {[]string{filepath.Join(linked, "a.go"), filepath.Join(mainDir, ".git", "x")}, mustEvalSymlinks(t, linked)},
	}
	for name, tc := range cases {
		got := turnWorktree(t.Context(), mainDir, tc.edited)
		if tc.want == "" {
			assert.Empty(t, got, name)
			continue
		}
		assert.Equal(t, tc.want, mustEvalSymlinks(t, got), name)
	}
}

// A path reached through a symlink resolves to the worktree it is in, and the
// root returned is the one git printed.
func TestTurnWorktree_ResolvesSymlinkedPaths(t *testing.T) {
	mainDir := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", linked, "-b", "linked")
	chdirWorktree(t, mainDir)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(linked, alias))

	got := turnWorktree(t.Context(), mainDir, []string{filepath.Join(alias, "a.go")})
	assert.Equal(t, mustEvalSymlinks(t, linked), mustEvalSymlinks(t, got))
	printed, err := filepath.Abs(got)
	require.NoError(t, err)
	assert.Equal(t, printed, got, "the root should be git's absolute path")
}

// The session moves to the worktree its turn edited, its stored prompts move
// with it (appended to any already there), and a worktree without a valid
// .entire leaves it where it is.
func TestSettleTurnWorktree_MovesTheSessionAndItsPrompts(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "settle")
	// No pending work: the home turn's file was committed.
	require.NoError(t, MutateSessionState(t.Context(), "settle", func(state *SessionState) error {
		state.StepCount, state.FilesTouched, state.TouchedFileHashes = 0, nil, nil
		return nil
	}))
	writeStoredCopy(t, mainDir, "settle", "home")
	testutil.WriteFile(t, linkedDir, filepath.Join(paths.SessionMetadataDirFromSessionID("settle"), paths.PromptFileName), "earlier prompt in linked")

	got := SettleTurnWorktree(t.Context(), "settle", mainDir, []string{filepath.Join(linkedDir, "w.go")})
	assert.Equal(t, mustEvalSymlinks(t, linkedDir), mustEvalSymlinks(t, got))
	state := mustLoad(t, s, "settle")
	assert.True(t, isSessionHomeWorktree(got, state), "home = %s", state.WorktreePath)
	assert.Equal(t, testutil.GetHeadHash(t, linkedDir), state.BaseCommit)
	assert.Equal(t, "linked-branch", state.Branch)

	prompts, err := os.ReadFile(filepath.Join(linkedDir, filepath.FromSlash(paths.SessionMetadataDirFromSessionID("settle")), paths.PromptFileName))
	require.NoError(t, err)
	assert.Equal(t, "earlier prompt in linked\n\n---\n\nprompt from home", string(prompts))
	_, err = os.Stat(filepath.Join(mainDir, filepath.FromSlash(paths.SessionMetadataDirFromSessionID("settle")), paths.PromptFileName))
	assert.True(t, os.IsNotExist(err), "the old home's copy should be released")
}

func TestSettleTurnWorktree_StaysWithPendingWorkOrWithoutEntire(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "settle-stay")
	// home.go from the home turn is still pending.
	got := SettleTurnWorktree(t.Context(), "settle-stay", mainDir, []string{filepath.Join(linkedDir, "w.go")})
	assert.Equal(t, mainDir, got)
	assert.True(t, isSessionHomeWorktree(mustEvalSymlinks(t, mainDir), mustLoad(t, s, "settle-stay")))

	// No pending work, but the linked worktree's .entire is not a directory.
	require.NoError(t, MutateSessionState(t.Context(), "settle-stay", func(state *SessionState) error {
		state.StepCount, state.FilesTouched = 0, nil
		return nil
	}))
	require.NoError(t, os.RemoveAll(filepath.Join(linkedDir, ".entire")))
	require.NoError(t, os.WriteFile(filepath.Join(linkedDir, ".entire"), []byte("not a dir"), 0o600))
	got = SettleTurnWorktree(t.Context(), "settle-stay", mainDir, []string{filepath.Join(linkedDir, "w.go")})
	assert.Equal(t, mainDir, got)
	assert.True(t, isSessionHomeWorktree(mustEvalSymlinks(t, mainDir), mustLoad(t, s, "settle-stay")))
}

// SaveStep given the turn's worktree hashes its files there and takes its
// HEAD as the base.
func TestSaveStep_InTheTurnsWorktree(t *testing.T) {
	s, _, linkedDir := homedSession(t, "step-elsewhere")
	require.NoError(t, MutateSessionState(t.Context(), "step-elsewhere", func(state *SessionState) error {
		state.StepCount, state.FilesTouched, state.TouchedFileHashes = 0, nil, nil
		state.WorktreePath = mustEvalSymlinks(t, linkedDir)
		return nil
	}))
	testutil.WriteFile(t, linkedDir, "w.go", "package w")
	require.NoError(t, s.SaveStep(t.Context(), StepContext{
		SessionID:     "step-elsewhere",
		WorktreeRoot:  mustEvalSymlinks(t, linkedDir),
		ModifiedFiles: []string{"w.go"},
		MetadataDir:   paths.SessionMetadataDirFromSessionID("step-elsewhere"),
		CommitMessage: "Checkpoint",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))
	state := mustLoad(t, s, "step-elsewhere")
	assert.Contains(t, state.FilesTouched, "w.go")
	assert.NotEmpty(t, state.TouchedFileHashes["w.go"])
	assert.Equal(t, testutil.GetHeadHash(t, linkedDir), state.BaseCommit)
}
