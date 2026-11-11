package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// newAgentInSubdirRepo returns a repo that tracks hello.txt and sub/hello.txt,
// with hello.txt left dirty by the user, its worktree root with symlinks
// resolved, and a mock agent whose transcript names hello.txt, as Pi's write
// tool and Codex's apply_patch do for sub/hello.txt when the agent runs in
// sub/. It chdirs into the repo.
func newAgentInSubdirRepo(t *testing.T) (string, *mockAnalyzerAgent) {
	t.Helper()
	dir := t.TempDir()
	releaseSharedRoots(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "hello.txt", "root\n")
	testutil.WriteFile(t, dir, "sub/hello.txt", "sub\n")
	testutil.GitAdd(t, dir, "hello.txt", "sub/hello.txt")
	testutil.GitCommit(t, dir, "Add files")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	root, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	testutil.WriteFile(t, dir, "hello.txt", "root, the user's edit\n")
	return root, &mockAnalyzerAgent{
		mockLifecycleAgent: &mockLifecycleAgent{
			name:           "mock-analyzer",
			agentType:      "Mock Analyzer Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}`),
		},
		analyzerFiles: []string{"hello.txt"},
	}
}

func writeTestTranscript(t *testing.T) string {
	t.Helper()
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))
	return transcriptPath
}

// TestHandleLifecycleTurnEnd_PlacesNamesInTheAgentsDirectory: the root's
// hello.txt, dirty since before the prompt, must not be taken for the
// transcript's hello.txt. Not parallel: t.Chdir.
func TestHandleLifecycleTurnEnd_PlacesNamesInTheAgentsDirectory(t *testing.T) {
	root, ag := newAgentInSubdirRepo(t)
	ctx := context.Background()
	sessionID := "sess-agent-cwd"
	transcriptPath := writeTestTranscript(t)

	require.NoError(t, handleLifecycleTurnStart(ctx, ag, &agent.Event{
		Type: agent.TurnStart, SessionID: sessionID, SessionRef: transcriptPath, Prompt: "edit hello.txt", Timestamp: time.Now(),
	}))
	testutil.WriteFile(t, root, "sub/hello.txt", "sub, edited\n")
	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, &agent.Event{
		Type: agent.TurnEnd, SessionID: sessionID, SessionRef: transcriptPath, CWD: filepath.Join(root, "sub"), Timestamp: time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{"sub/hello.txt"}, state.FilesTouched)
}

// TestHandleLifecycleSubagentEnd_PlacesNamesInTheAgentsDirectory is the task
// route. Not parallel: t.Chdir.
func TestHandleLifecycleSubagentEnd_PlacesNamesInTheAgentsDirectory(t *testing.T) {
	root, ag := newAgentInSubdirRepo(t)
	ctx := context.Background()
	sessionID := "sess-agent-cwd-task"
	toolUseID := "toolu-agent-cwd-01"
	transcriptPath := writeTestTranscript(t)

	require.NoError(t, CapturePreTaskState(ctx, "", toolUseID))
	testutil.WriteFile(t, root, "sub/hello.txt", "sub, edited\n")
	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, &agent.Event{
		Type: agent.SubagentEnd, SessionID: sessionID, SessionRef: transcriptPath, ToolUseID: toolUseID,
		CWD: filepath.Join(root, "sub"), Timestamp: time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	require.Equal(t, []string{"sub/hello.txt"}, rec.Files)
}
