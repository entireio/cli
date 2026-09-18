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

// TestCommitLinking_AgentCommitInForeignWorktreeReHomesSession covers the
// launch-in-parent, work-in-worktree shape. The agent's first turn-start ran in
// the main checkout, so the session is homed there; the worktree it then works
// in is created during the session, and every edit and commit lands there.
// Process ancestry links the agent's own commit (guest-linked, as before). After
// that commit the session must follow its agent: it is re-homed to the worktree
// it committed in, so a later commit there that does NOT descend from the agent
// process finds the session by exact worktree match instead of falling into
// the multi-worktree ambiguity refusal.
func TestCommitLinking_AgentCommitInForeignWorktreeReHomesSession(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)

	// Another agent's live session in a sibling worktree makes the path
	// fallback ambiguous (candidates span two worktrees), as in any clone
	// with several agents running at once.
	other := worktreeEnv(t, parent, "other")
	require.NoError(t, other.SimulateUserPromptSubmit("other-agent-session"))
	disownSession(t, parent, "other-agent-session")

	// The agent under test starts in the parent and only then moves into a
	// worktree; its hooks never ran there.
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

	// A commit in that worktree from a process that is not the agent's
	// descendant (a script, an IDE) now finds the session by exact match.
	// Before re-homing it was refused as ambiguous between the parent and the
	// other live worktree.
	disownSession(t, parent, sess.ID)
	feature.WriteFile("feature2.txt", "more work\n")
	feature.GitCommitWithShadowHooksAsAgent("Second commit in the worktree", "feature2.txt")
	require.Contains(t, headMessage(t, feature.RepoDir), "Entire-Checkpoint:",
		"after re-homing, the worktree's own commits link by exact match")
}

// worktreeEnv adds a linked worktree to parent's repository and returns a
// TestEnv whose hooks, commits and files all target that worktree. Entire is
// initialised there because `.entire/` is gitignored and so absent from a fresh
// checkout.
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

// disownSession records an owner process that is not in this test's ancestry,
// so commits made by the test are no longer attributable to the session by
// process identity: the shape of a human at a terminal, or a script, committing
// in the session's worktree.
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
