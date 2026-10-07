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
