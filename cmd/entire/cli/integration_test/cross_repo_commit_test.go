//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// Cross-repository agent commits: a Claude Code session homed in repo A whose
// agent commits in another Entire-enabled repo B. See "Agent commits in
// another repository" in docs/architecture/sessions-and-checkpoints.md.

const crossRepoSessionID = "0f0e0d0c-1111-4222-8333-444455556666"

type crossRepoFixture struct {
	a, b    *TestEnv
	cache   []string
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
	b.ExtraEnv = append(b.ExtraEnv, cache...)
	installRealCommitHooks(b)

	b.WriteFile("b.txt", "b\n")
	a.WriteFile("a.txt", "a\n")
	hooks, s := startSession(t, a, cache, crossRepoSessionID,
		FileChange{Path: filepath.Join(a.RepoDir, "a.txt"), Content: "a\n"},
		FileChange{Path: filepath.Join(b.RepoDir, "b.txt"), Content: "b\n"})
	return &crossRepoFixture{a: a, b: b, cache: cache, hooks: hooks, session: s}
}

// startSession starts a turn of session id homed in env, recorded in cache,
// whose transcript makes changes (absolute paths, in any repository).
func startSession(t *testing.T, env *TestEnv, cache []string, id string, changes ...FileChange) (*HookRunner, *Session) {
	t.Helper()
	env.ExtraEnv = append(env.ExtraEnv, cache...)
	hooks := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	hooks.Env = cache
	s := &Session{
		ID:                id,
		TranscriptPath:    filepath.Join(env.RepoDir, ".entire", "tmp", id+".jsonl"),
		TranscriptBuilder: NewTranscriptBuilder(),
		env:               env,
	}
	const prompt = "edit files here and in the other repo, then commit"
	require.NoError(t, hooks.SimulateUserPromptSubmitWithPromptAndTranscriptPath(s.ID, prompt, s.TranscriptPath))
	s.CreateTranscript(prompt, changes)
	return hooks, s
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

// moveOwnerOutOfAncestry gives session id an owner that is not an ancestor of
// the commits these tests make. In the harness every recorded owner is the test
// binary, which every hook descends from; real sessions in different agents
// are owned by different processes, so only the caller variable can name them.
func moveOwnerOutOfAncestry(t *testing.T, env *TestEnv, id string) {
	t.Helper()
	state, err := env.GetSessionState(id)
	require.NoError(t, err)
	require.NotNil(t, state)
	state.Owner = &proclive.Identity{PID: 999999, Start: "never", Host: "elsewhere", Name: "claude"}
	require.NoError(t, env.WriteSessionState(id, state))
}

// Two foreign sessions in different agents are recorded (homed in A and in C)
// and both edited files in B. Each agent's commit links only its own session.
func TestCrossRepoCommit_TwoForeignSessionsOnlyCommitterLinks(t *testing.T) {
	t.Parallel()
	f := newCrossRepoFixture(t)
	c := NewFeatureBranchEnv(t)
	const otherID = "1f1e1d1c-2222-4333-8444-555566667777"
	f.b.WriteFile("c.txt", "c\n")
	_, _ = startSession(t, c, f.cache, otherID,
		FileChange{Path: filepath.Join(f.b.RepoDir, "c.txt"), Content: "c\n"})
	moveOwnerOutOfAncestry(t, f.a, f.session.ID)
	moveOwnerOutOfAncestry(t, c, otherID)
	otherBefore, err := c.GetSessionState(otherID)
	require.NoError(t, err)

	agentGit(f.b, f.session.ID, "add", "b.txt")
	agentGit(f.b, f.session.ID, "commit", "-m", "add b")
	cp := f.b.GetCheckpointIDFromCommitMessage(f.b.GetHeadHash())
	require.NotEmpty(t, cp)
	require.Equal(t, f.session.ID, readSessionMetadata(t, f.b, cp).SessionID)

	otherAfter, err := c.GetSessionState(otherID)
	require.NoError(t, err)
	require.Equal(t, otherBefore.CheckpointTranscriptStart, otherAfter.CheckpointTranscriptStart,
		"the other foreign session is not condensed")

	// Then the other agent commits its own file in B: its own checkpoint.
	agentGit(f.b, otherID, "add", "c.txt")
	agentGit(f.b, otherID, "commit", "-m", "add c")
	cp2 := f.b.GetCheckpointIDFromCommitMessage(f.b.GetHeadHash())
	require.NotEmpty(t, cp2)
	require.NotEqual(t, cp, cp2)
	meta := readSessionMetadata(t, f.b, cp2)
	require.Equal(t, otherID, meta.SessionID)
	require.Equal(t, []string{"c.txt"}, meta.FilesTouched)
}

// B has its own active session with work in the commit: it links the commit
// the usual way, and the foreign session in A is not condensed.
func TestCrossRepoCommit_LocalSessionWithContentTakesCommit(t *testing.T) {
	t.Parallel()
	f := newCrossRepoFixture(t)
	const localID = "2f2e2d2c-3333-4444-8555-666677778888"
	_, _ = startSession(t, f.b, f.cache, localID,
		FileChange{Path: filepath.Join(f.b.RepoDir, "b.txt"), Content: "b\n"})
	before, err := f.a.GetSessionState(f.session.ID)
	require.NoError(t, err)

	agentGit(f.b, localID, "add", "b.txt")
	agentGit(f.b, localID, "commit", "-m", "add b")
	cp := f.b.GetCheckpointIDFromCommitMessage(f.b.GetHeadHash())
	require.NotEmpty(t, cp)
	require.Equal(t, localID, readSessionMetadata(t, f.b, cp).SessionID)

	after, err := f.a.GetSessionState(f.session.ID)
	require.NoError(t, err)
	require.Equal(t, before.CheckpointTranscriptStart, after.CheckpointTranscriptStart,
		"the foreign session is not condensed into B's commit")
}

// B's origin has a different owner than A's: the commit is left unlinked and
// the agent's git output says why.
func TestCrossRepoCommit_DifferentOwnerNotLinkedWithNotice(t *testing.T) {
	t.Parallel()
	f := newCrossRepoFixture(t)
	testutil.RunGit(t, f.a.RepoDir, "remote", "add", "origin", "git@github.com:acme/a.git")
	testutil.RunGit(t, f.b.RepoDir, "remote", "add", "origin", "git@github.com:other/b.git")
	f.a.setGitConfigBaseline()
	f.b.setGitConfigBaseline()
	before, err := f.a.GetSessionState(f.session.ID)
	require.NoError(t, err)

	agentGit(f.b, f.session.ID, "add", "b.txt")
	cmd := execx.NonInteractive(t.Context(), "git", "commit", "-m", "add b")
	cmd.Dir = f.b.RepoDir
	cmd.Env = f.b.gitHookEnv("CLAUDE_CODE_SESSION_ID=" + f.session.ID)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	require.Contains(t, string(out), "This commit is not linked to agent session "+f.session.ID)
	require.Empty(t, f.b.GetCheckpointIDFromCommitMessage(f.b.GetHeadHash()))
	after, err := f.a.GetSessionState(f.session.ID)
	require.NoError(t, err)
	require.Equal(t, before.CheckpointTranscriptStart, after.CheckpointTranscriptStart)
}
