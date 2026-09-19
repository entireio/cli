//go:build integration

package integration

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A session homed in the main checkout whose agent commits in a worktree is
// re-homed there, so a later commit from a non-agent process links by exact
// match instead of hitting the multi-worktree refusal.
func TestCommitLinking_AgentCommitInForeignWorktreeReHomesSession(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)

	// A second live worktree makes the path fallback ambiguous.
	other := worktreeEnv(t, parent, "other")
	require.NoError(t, other.SimulateUserPromptSubmit("other-agent-session"))
	disownSession(t, parent, "other-agent-session")

	// The agent starts in the parent; its hooks never run in the worktree.
	sess := parent.NewSession()
	prompt := "Implement the feature in a worktree"
	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
	feature := worktreeEnv(t, parent, "feature")
	feature.WriteFile("feature.txt", "agent work\n")
	sess.CreateTranscript(prompt, []FileChange{{Path: "feature.txt", Content: "agent work\n"}})
	feature.GitCommitWithShadowHooksAsAgent("Agent commit in the worktree", "feature.txt")
	require.Contains(t, headMessage(t, feature.RepoDir), "Entire-Checkpoint:",
		"the agent's own commit is linked through process ancestry")

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	wantWorktreeID, err := paths.GetWorktreeID(feature.RepoDir)
	require.NoError(t, err)
	require.Equal(t, feature.RepoDir, state.WorktreePath,
		"session must follow its agent into the worktree it committed in")
	require.Equal(t, wantWorktreeID, state.WorktreeID)

	// A non-agent process committing there now finds the session by exact match.
	disownSession(t, parent, sess.ID)
	feature.WriteFile("feature2.txt", "more work\n")
	feature.GitCommitWithShadowHooksAsAgent("Second commit in the worktree", "feature2.txt")
	require.Contains(t, headMessage(t, feature.RepoDir), "Entire-Checkpoint:",
		"after re-homing, the worktree's own commits link by exact match")
}

// worktreeEnv adds a linked worktree, with Entire initialised, as a TestEnv.
func worktreeEnv(t *testing.T, parent *TestEnv, name string) *TestEnv {
	t.Helper()
	base := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	dir := filepath.Join(base, name)
	testutil.RunGit(t, parent.RepoDir, "worktree", "add", "-b", "wt/"+name, dir)
	env := *parent
	env.RepoDir = dir
	env.InitEntire()
	return &env
}

// disownSession records an owner outside this test's ancestry.
func disownSession(t *testing.T, env *TestEnv, sessionID string) {
	t.Helper()
	state, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state, "session %s must exist before it can be disowned", sessionID)
	state.Owner = &proclive.Identity{PID: 999999, Start: "0", Host: "elsewhere.invalid", Name: "foreign-agent"}
	require.NoError(t, env.WriteSessionState(sessionID, state))
}

func headMessage(t *testing.T, dir string) string {
	t.Helper()
	return testutil.RunGit(t, dir, "log", "-1", "--format=%B")
}
