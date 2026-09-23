package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestHandleLifecycleTurnEnd_RepeatableStopTokenUsage(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "README.md", "initial\n")
	testutil.GitAdd(t, workDir, "README.md")
	testutil.GitCommit(t, workDir, "initial")
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	ctx := context.Background()
	const sessionID = "repeatable-token-usage"
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, nil, 0o600))
	ag := &claudecode.ClaudeCodeAgent{}
	start := func() {
		require.NoError(t, handleLifecycleTurnStart(ctx, ag, &agent.Event{Type: agent.TurnStart, SessionID: sessionID, SessionRef: transcriptPath, Prompt: "continue"}))
	}
	start()
	message := func(id, response string, input, output int) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":"%s","content":"%s","usage":{"input_tokens":%d,"output_tokens":%d}}}`+"\n", id, response, input, output)
	}
	const secondResponse = "Second."
	first := message("first", "First.", 10, 5)
	second := first + message("second", secondResponse, 10, 5)
	subagentsDir := paths.SubagentsDir(filepath.Dir(transcriptPath), sessionID)
	require.NoError(t, os.MkdirAll(subagentsDir, 0o700))
	subagentPath := filepath.Join(subagentsDir, "agent-worker.jsonl")
	steps := []struct {
		data, response                      string
		nextPrompt                          bool
		input, output, calls, subagentInput int
	}{
		{first, "First.", false, 10, 5, 1, 3},
		{message("first", "Corrected.", 5, 2), "Corrected.", false, 10, 5, 1, 4},
		{second, secondResponse, false, 20, 10, 2, 5},
		{second, secondResponse, false, 20, 10, 2, 6},
		{second + message("third", "Third.", 10, 5), "Third.", true, 30, 15, 3, 7},
	}
	for i, step := range steps {
		if step.nextPrompt {
			start()
		}
		require.NoError(t, os.WriteFile(transcriptPath, []byte(step.data), 0o600))
		require.NoError(t, os.WriteFile(subagentPath, []byte(message("worker", "Worker.", step.subagentInput, 1)), 0o600))
		testutil.WriteFile(t, workDir, "README.md", fmt.Sprintf("change %d\n", i))
		require.NoError(t, handleLifecycleTurnEnd(ctx, ag, &agent.Event{Type: agent.TurnEnd, SessionID: sessionID, SessionRef: transcriptPath, FinalResponse: &step.response}))
		state, err := strategy.LoadSessionState(ctx, sessionID)
		require.NoError(t, err)
		require.NotNil(t, state.TokenUsage)
		require.Equal(t, step.input, state.TokenUsage.InputTokens, "session input after Stop %d", i+1)
		require.Equal(t, step.output, state.TokenUsage.OutputTokens)
		require.Equal(t, step.calls, state.TokenUsage.APICallCount)
		require.Equal(t, step.subagentInput, state.TokenUsage.SubagentTokens.InputTokens)
		require.Equal(t, state.TokenUsage, state.CheckpointTokenUsage)
	}
}
