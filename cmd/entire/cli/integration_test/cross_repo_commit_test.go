//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// Cross-repository agent commits: a Claude Code session homed in repo A whose
// agent commits in another Entire-enabled repo B. See "Agent commits in
// another repository" in docs/architecture/sessions-and-checkpoints.md.

const crossRepoSessionID = "0f0e0d0c-1111-4222-8333-444455556666"

type crossRepoFixture struct {
	a, b    *TestEnv
	hooks   *HookRunner // A's Claude hooks
	session *Session
}

// newCrossRepoFixture starts a turn of a session homed in A whose transcript
// writes a.txt in A and b.txt in B by absolute path. A and B share a per-test
// cache (the session-home index lives there; TestMain blocks the shared one),
// and B has real git hooks installed, so its commits run them as git does.
func newCrossRepoFixture(t *testing.T) *crossRepoFixture {
	t.Helper()
	a := NewFeatureBranchEnv(t)
	b := NewFeatureBranchEnv(t)
	cache := []string{"XDG_CACHE_HOME=" + t.TempDir()}
	a.ExtraEnv = append(a.ExtraEnv, cache...)
	b.ExtraEnv = append(b.ExtraEnv, cache...)
	installRealCommitHooks(b)

	hooks := NewHookRunner(a.RepoDir, a.ClaudeProjectDir, t)
	hooks.Env = cache
	s := &Session{
		ID:                crossRepoSessionID,
		TranscriptPath:    filepath.Join(a.RepoDir, ".entire", "tmp", crossRepoSessionID+".jsonl"),
		TranscriptBuilder: NewTranscriptBuilder(),
		env:               a,
	}
	const prompt = "write a.txt here and b.txt in the other repo, then commit both"
	require.NoError(t, hooks.SimulateUserPromptSubmitWithPromptAndTranscriptPath(s.ID, prompt, s.TranscriptPath))
	a.WriteFile("a.txt", "a\n")
	b.WriteFile("b.txt", "b\n")
	s.CreateTranscript(prompt, []FileChange{
		{Path: filepath.Join(a.RepoDir, "a.txt"), Content: "a\n"},
		{Path: filepath.Join(b.RepoDir, "b.txt"), Content: "b\n"},
	})
	return &crossRepoFixture{a: a, b: b, hooks: hooks, session: s}
}

// installRealCommitHooks writes the commit hooks `entire enable` installs, run
// through the test binary rather than whatever `entire` is on PATH.
func installRealCommitHooks(env *TestEnv) {
	env.T.Helper()
	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
	require.NoError(env.T, os.MkdirAll(hooksDir, 0o755))
	for name, args := range map[string]string{
		"prepare-commit-msg": `prepare-commit-msg "$1" "$2"`,
		"commit-msg":         `commit-msg "$1"`,
		"post-commit":        `post-commit`,
	} {
		script := fmt.Sprintf("#!/bin/sh\n%q hooks git %s || true\n", getTestBinary(), args)
		require.NoError(env.T, os.WriteFile(filepath.Join(hooksDir, name), []byte(script), 0o755))
	}
}

// agentGit runs git in env's repo the way an agent's shell tool does: no
// controlling terminal and the agent's caller-session variable set.
func agentGit(env *TestEnv, sessionID string, args ...string) {
	env.T.Helper()
	cmd := execx.NonInteractive(env.T.Context(), "git", args...)
	cmd.Dir = env.RepoDir
	cmd.Env = env.gitHookEnv("CLAUDE_CODE_SESSION_ID=" + sessionID)
	out, err := cmd.CombinedOutput()
	require.NoError(env.T, err, "git %v: %s", args, out)
}

func TestCrossRepoCommit_LinksAgentCommitInOtherRepo(t *testing.T) {
	t.Parallel()
	f := newCrossRepoFixture(t)
	before, err := f.a.GetSessionState(f.session.ID)
	require.NoError(t, err)
	require.NotNil(t, before)

	agentGit(f.b, f.session.ID, "add", "b.txt")
	agentGit(f.b, f.session.ID, "commit", "-m", "add b")
	cpB := f.b.GetCheckpointIDFromCommitMessage(f.b.GetHeadHash())
	require.NotEmpty(t, cpB, "the agent's commit in B carries a trailer")

	meta := readSessionMetadata(t, f.b, cpB)
	require.Equal(t, f.session.ID, meta.SessionID)
	require.Equal(t, []string{"b.txt"}, meta.FilesTouched)

	after, err := f.a.GetSessionState(f.session.ID)
	require.NoError(t, err)
	require.Greater(t, after.CheckpointTranscriptStart, before.CheckpointTranscriptStart,
		"A's transcript window moves past what B's checkpoint holds")
	require.Equal(t, before.LastCheckpointID, after.LastCheckpointID)
	stateB, err := f.b.GetSessionState(f.session.ID)
	require.NoError(t, err)
	require.Nil(t, stateB, "B gains no session state")

	// The agent then commits at home: A's own checkpoint, after B's window.
	f.a.GitCommitWithHooksAsAgent("add a", "a.txt")
	cpA := f.a.GetCheckpointIDFromCommitMessage(f.a.GetHeadHash())
	require.NotEmpty(t, cpA)
	require.NotEqual(t, cpB, cpA)
	metaA := readSessionMetadata(t, f.a, cpA)
	require.NotContains(t, metaA.FilesTouched, "b.txt")
	require.GreaterOrEqual(t, metaA.CheckpointTranscriptStart, after.CheckpointTranscriptStart)

	// Turn end in A never writes B's checkpoint into A's store.
	require.NoError(t, f.hooks.SimulateStop(f.session.ID, f.session.TranscriptPath))
	_, found := f.a.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(cpB))
	require.False(t, found)
}
