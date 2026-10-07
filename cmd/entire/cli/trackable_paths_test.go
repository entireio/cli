package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// The tests in this file assert on git ignore results. The developer's global
// git config (a global excludes file) cannot leak into them: the package's
// TestMain isolates git config process-wide with gitenv.IsolateMain, which
// spawned git commands inherit.

// setupIgnoreRouteRepo creates a repo whose .gitignore excludes ignored.env,
// chdirs into it, and returns the repo dir (symlinks resolved) and a mock agent
// whose transcript analyzer names both the ignored file and a normal one.
func setupIgnoreRouteRepo(t *testing.T) (string, *mockAnalyzerAgent) {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, ".gitignore", "ignored.env\n")
	testutil.GitAdd(t, tmpDir, ".gitignore")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	resolvedDir, err := filepath.EvalSymlinks(tmpDir)
	require.NoError(t, err)
	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: &mockLifecycleAgent{
			name:           "mock-analyzer",
			agentType:      "Mock Analyzer Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}`),
		},
		analyzerFiles: []string{filepath.Join(resolvedDir, "agent.txt"), filepath.Join(resolvedDir, "ignored.env")},
	}
	return resolvedDir, ag
}

func writeAgentFiles(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.env"), []byte("SECRET=1"), 0o600))
}

// An agent edit to a gitignored file can never be committed, so it must never
// enter FilesTouched: it would keep the session pending forever. Turn-end
// route. Not parallel: t.Chdir.
func TestHandleLifecycleTurnEnd_DropsIgnoredFiles(t *testing.T) {
	dir, ag := setupIgnoreRouteRepo(t)
	ctx := context.Background()
	sessionID := "sess-ignored-turn-end"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	require.NoError(t, handleLifecycleTurnStart(ctx, ag, &agent.Event{
		Type: agent.TurnStart, SessionID: sessionID, SessionRef: transcriptPath, Prompt: "write files", Timestamp: time.Now(),
	}))
	writeAgentFiles(t, dir)
	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, &agent.Event{
		Type: agent.TurnEnd, SessionID: sessionID, SessionRef: transcriptPath, Timestamp: time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "agent.txt")
	require.NotContains(t, state.FilesTouched, "ignored.env")
	require.NotContains(t, state.TouchedFileHashes, "ignored.env")
}

// Task-record route (subagent end). Not parallel: t.Chdir.
func TestHandleLifecycleSubagentEnd_DropsIgnoredFiles(t *testing.T) {
	dir, ag := setupIgnoreRouteRepo(t)
	ctx := context.Background()
	sessionID := "sess-ignored-task"
	toolUseID := "toolu-ignored-01"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	require.NoError(t, CapturePreTaskState(ctx, toolUseID))
	writeAgentFiles(t, dir)
	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, &agent.Event{
		Type: agent.SubagentEnd, SessionID: sessionID, SessionRef: transcriptPath, ToolUseID: toolUseID, Timestamp: time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "agent.txt")
	require.NotContains(t, state.FilesTouched, "ignored.env")
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	require.NotContains(t, rec.Files, "ignored.env")
}

// Per-tool route (RecordFilesTouched). Not parallel: t.Chdir.
func TestHandleLifecycleToolUse_DropsIgnoredFiles(t *testing.T) {
	dir, ag := setupIgnoreRouteRepo(t)
	ctx := context.Background()
	sessionID := "sess-ignored-tool-use"
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID: sessionID, BaseCommit: "abc123", StartedAt: time.Now(), Phase: session.PhaseActive,
	}))
	writeAgentFiles(t, dir)

	require.NoError(t, handleLifecycleToolUse(ctx, ag, &agent.Event{
		Type:          agent.ToolUse,
		SessionID:     sessionID,
		CWD:           dir,
		ModifiedFiles: []string{"agent.txt", "ignored.env"},
		Timestamp:     time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{"agent.txt"}, state.FilesTouched)
}

// addSubmodule creates a separate repo and adds it as submodule "sub" of dir
// with a real `git submodule add`, committing the gitlink.
func addSubmodule(t *testing.T, dir string) string {
	t.Helper()
	subSrc := t.TempDir()
	testutil.InitRepo(t, subSrc)
	testutil.WriteFile(t, subSrc, "lib.txt", "v1\n")
	testutil.GitAdd(t, subSrc, "lib.txt")
	testutil.GitCommit(t, subSrc, "lib v1")
	testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	testutil.RunGit(t, dir, "commit", "-q", "-m", "add submodule")
	sub := filepath.Join(dir, "sub")
	// The submodule is a fresh clone: it does not inherit the local identity
	// testutil.InitRepo wrote into subSrc, and the test process's git config
	// is isolated, so give the clone the same identity before committing in it.
	// Without this, git falls back to host-derived identity, which works on a
	// developer machine and fails on CI.
	testutil.RunGit(t, sub, "config", "user.name", "Test User")
	testutil.RunGit(t, sub, "config", "user.email", "test@example.com")
	testutil.RunGit(t, sub, "config", "commit.gpgsign", "false")
	return sub
}

// A dirty submodule pointer reaches turn end through the git-status merge.
// It is a gitlink, not a file of the session's work, so it must never enter
// FilesTouched. Not parallel: t.Chdir.
func TestHandleLifecycleTurnEnd_DropsSubmoduleGitlink(t *testing.T) {
	dir, ag := setupIgnoreRouteRepo(t)
	ag.analyzerFiles = []string{filepath.Join(dir, "agent.txt")}
	sub := addSubmodule(t, dir)
	ctx := context.Background()
	sessionID := "sess-submodule-turn-end"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	require.NoError(t, handleLifecycleTurnStart(ctx, ag, &agent.Event{
		Type: agent.TurnStart, SessionID: sessionID, SessionRef: transcriptPath, Prompt: "work", Timestamp: time.Now(),
	}))
	// Move the submodule's checked-out commit so the parent sees `M sub`.
	testutil.WriteFile(t, sub, "lib.txt", "v2\n")
	testutil.RunGit(t, sub, "-c", "user.useConfigOnly=true", "commit", "-q", "-am", "lib v2") // useConfigOnly: never fall back to a host-derived identity
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent"), 0o600))
	require.Contains(t, testutil.RunGit(t, dir, "--no-optional-locks", "status", "--porcelain"), " M sub")

	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, &agent.Event{
		Type: agent.TurnEnd, SessionID: sessionID, SessionRef: transcriptPath, Timestamp: time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "agent.txt")
	require.NotContains(t, state.FilesTouched, "sub")
}

// Task-record route twin of TestHandleLifecycleTurnEnd_DropsSubmoduleGitlink.
// Not parallel: t.Chdir.
func TestHandleLifecycleSubagentEnd_DropsSubmoduleGitlink(t *testing.T) {
	dir, ag := setupIgnoreRouteRepo(t)
	ag.analyzerFiles = []string{filepath.Join(dir, "agent.txt")}
	sub := addSubmodule(t, dir)
	ctx := context.Background()
	sessionID := "sess-submodule-task"
	toolUseID := "toolu-submodule-01"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	require.NoError(t, CapturePreTaskState(ctx, toolUseID))
	testutil.WriteFile(t, sub, "lib.txt", "v2\n")
	testutil.RunGit(t, sub, "-c", "user.useConfigOnly=true", "commit", "-q", "-am", "lib v2") // useConfigOnly: never fall back to a host-derived identity
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent"), 0o600))
	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, &agent.Event{
		Type: agent.SubagentEnd, SessionID: sessionID, SessionRef: transcriptPath, ToolUseID: toolUseID, Timestamp: time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "agent.txt")
	require.NotContains(t, state.FilesTouched, "sub")
}
