package strategy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1965: when a coding agent commits in an Entire-enabled repo but no
// session anywhere in the repository has recorded recent activity (e.g. the
// agent was launched from a bare-clone layout's shared root, outside any
// worktree, so its hooks never fired), PrepareCommitMsg used to only log a
// debug line invisible to a normal user before silently skipping the commit.
// These tests exercise the real hook handler (not a hand-mocked call) end to
// end and assert a visible stderr warning now appears.

// writeEnabledSettings writes a minimal .entire/settings.json with
// "enabled": true, matching what `entire enable` produces and what the
// PersistentPreRun gate in hooks_git_cmd.go requires before any git-hook
// command reaches the strategy layer in a real invocation.
func writeEnabledSettings(t *testing.T, dir string) {
	t.Helper()
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled":true}`),
		0o644,
	))
}

// TestPrepareCommitMsg_AgentCommitNoSession_WarnsVisibly is the core
// regression test for #1965. Setup: a real temp repo (testutil.InitRepo via
// setupGitRepo), Entire enabled, CLAUDECODE=1 (the real env marker Claude
// Code sets on every command it runs), and deliberately NO session state
// anywhere (.git/entire-sessions/ is never populated - no InitializeSession
// call). PrepareCommitMsg is invoked directly - the actual function a real
// prepare-commit-msg git hook invocation reaches - with a real commit message
// file, and stderr is captured through the package's real injectable
// stderrWriter (the same seam warnStaleEndedSessions already uses).
//
// On the fixed code this must produce a visible warning. Run against the
// pre-fix code (git stash the strategy fix), the captured buffer is empty:
// the only trace was the debug-level "prepare-commit-msg: no active
// sessions" log line, invisible to a user reading their terminal.
func TestPrepareCommitMsg_AgentCommitNoSession_WarnsVisibly(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	writeEnabledSettings(t, dir)
	t.Setenv("CLAUDECODE", "1")

	s := &ManualCommitStrategy{}

	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("fix: something\n"), 0o644))

	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	defer func() { stderrWriter = oldWriter }()

	err := s.PrepareCommitMsg(context.Background(), commitMsgFile, "")
	require.NoError(t, err)

	t.Logf("captured stderr: %q", buf.String())

	assert.Contains(t, buf.String(), "no active Entire session",
		"a coding-agent commit with no session recorded anywhere in the repo must print a visible stderr warning (issue #1965)")
	assert.Contains(t, buf.String(), "entire doctor",
		"the warning should point the user at `entire doctor`")
}

// TestPrepareCommitMsg_HumanCommitNoSession_NoWarn pins the other half of the
// enabled-but-no-session vs no-agent-at-all distinction: with Entire enabled,
// no session anywhere, but NO agent env marker set (an ordinary human running
// `git commit` by hand), the warning must stay silent. This is
// indistinguishable from "no agent running at all" and must never nag a
// human's own commit.
func TestPrepareCommitMsg_HumanCommitNoSession_NoWarn(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	writeEnabledSettings(t, dir)
	// Force-clear every known agent env marker: this test asserts the
	// no-agent-at-all case, and the test process itself may be running
	// inside an actual agent session (e.g. this very fix was developed under
	// Claude Code, which sets CLAUDECODE=1 in its own subprocess env) - that
	// ambient marker must not leak into "no agent" test coverage.
	t.Setenv("CLAUDECODE", "")
	t.Setenv("GEMINI_CLI", "")
	t.Setenv("COPILOT_CLI", "")
	t.Setenv("PI_CODING_AGENT", "")

	s := &ManualCommitStrategy{}

	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("fix: something\n"), 0o644))

	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	defer func() { stderrWriter = oldWriter }()

	err := s.PrepareCommitMsg(context.Background(), commitMsgFile, "")
	require.NoError(t, err)

	t.Logf("captured stderr: %q", buf.String())

	assert.Empty(t, buf.String(),
		"a human commit with no agent env marker must never warn, even with no session recorded")
}

// TestPrepareCommitMsg_AgentCommitWithSession_NoWarn is the mandatory
// regression guard: a real session with recent activity in THIS worktree
// (initialized via the real InitializeSession turn-start path, not a
// hand-built state) must suppress the warning entirely - PrepareCommitMsg
// takes the "sessions found" branch and never reaches the no-session warning
// at all.
func TestPrepareCommitMsg_AgentCommitWithSession_NoWarn(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	writeEnabledSettings(t, dir)
	t.Setenv("CLAUDECODE", "1")

	s := &ManualCommitStrategy{}

	sessionID := "test-session-1965-present"
	require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, "", "working on a fix", ""))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.LastInteractionTime, "InitializeSession's turn-start transition should stamp LastInteractionTime")

	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("fix: something\n"), 0o644))

	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	defer func() { stderrWriter = oldWriter }()

	err = s.PrepareCommitMsg(context.Background(), commitMsgFile, "")
	require.NoError(t, err)

	t.Logf("captured stderr: %q", buf.String())

	assert.Empty(t, buf.String(),
		"an agent commit with a recently-active session present must not warn")
}

// TestWarnIfAgentCommitHasNoSession_RateLimit pins the sentinel-file rate
// limit, mirroring TestWarnStaleEndedSessions_RateLimit's pattern.
func TestWarnIfAgentCommitHasNoSession_RateLimit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	t.Setenv("CLAUDECODE", "1")
	ctx := context.Background()

	s := &ManualCommitStrategy{}

	var buf bytes.Buffer
	s.warnIfAgentCommitHasNoSessionTo(ctx, &buf)
	assert.Contains(t, buf.String(), "no active Entire session")

	buf.Reset()
	s.warnIfAgentCommitHasNoSessionTo(ctx, &buf)
	assert.Empty(t, buf.String(), "second call within window must be suppressed")
}
