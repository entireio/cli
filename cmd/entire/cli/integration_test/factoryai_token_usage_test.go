//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// Factory AI Droid writes no usage into its transcript. Its token counts live
// in the .settings.json beside the transcript as a running session total, so
// each turn's tokens are that total minus its value at turn start. The totals
// below are real snapshots from one Droid 0.243.0 session.

// writeDroidSettings writes the session's .settings.json with a token total.
func writeDroidSettings(t *testing.T, transcriptPath string, input, output, cacheRead int) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model": "custom:Gemini-2.5-Pro-0",
		"tokenUsage": map[string]int{
			"inputTokens": input, "outputTokens": output, "cacheCreationTokens": 0,
			"cacheReadTokens": cacheRead, "thinkingTokens": 0,
		},
	})
	require.NoError(t, err)
	settingsPath := strings.TrimSuffix(transcriptPath, ".jsonl") + ".settings.json"
	require.NoError(t, os.WriteFile(settingsPath, raw, 0o600))
}

func TestFactoryDroidTokensComeFromSessionSettings(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	session := env.NewFactoryDroidSession()
	appendLines(t, session.TranscriptPath, `{"type":"session_start","id":"`+session.ID+`"}`)

	// Turn 1 changes no files.
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmitWithTranscript(session.ID, session.TranscriptPath))
	appendLines(t, session.TranscriptPath,
		`{"type":"message","id":"t1u","message":{"role":"user","content":[{"type":"text","text":"What is this repo?"}]}}`,
		`{"type":"message","id":"t1a","message":{"role":"assistant","content":[{"type":"text","text":"A test repository."}]}}`)
	writeDroidSettings(t, session.TranscriptPath, 8035, 175, 20095)

	// Twice: a repeated Stop must not count the turn again.
	require.NoError(t, env.SimulateFactoryDroidStop(session.ID, session.TranscriptPath))
	require.NoError(t, env.SimulateFactoryDroidStop(session.ID, session.TranscriptPath))
	require.Equal(t, &agent.TokenUsage{InputTokens: 8035, OutputTokens: 175, CacheReadTokens: 20095, APICallCount: 1},
		sessionTokenTotal(t, env, session.ID))

	// Turn 2 resumes the session and writes a file.
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmitWithTranscript(session.ID, session.TranscriptPath))
	env.WriteFile("feature.go", "package main\n")
	appendLines(t, session.TranscriptPath,
		`{"type":"message","id":"t2u","message":{"role":"user","content":[{"type":"text","text":"Add a feature"}]}}`,
		`{"type":"message","id":"t2a","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Create","input":{"file_path":"feature.go","content":"package main\n"}}]}}`,
		`{"type":"message","id":"t2r","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`,
		`{"type":"message","id":"t2b","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`)
	writeDroidSettings(t, session.TranscriptPath, 30149, 725, 60603)
	require.NoError(t, env.SimulateFactoryDroidStop(session.ID, session.TranscriptPath))

	// The session total is Droid's own running total, counted once.
	require.Equal(t, &agent.TokenUsage{InputTokens: 30149, OutputTokens: 725, CacheReadTokens: 60603, APICallCount: 3},
		sessionTokenTotal(t, env, session.ID))

	// Both turns precede the first commit, so its checkpoint holds both.
	checkpointID, usage := commitCheckpointTokens(t, env, "", "Add feature", "feature.go")
	require.Equal(t, 30149, usage.InputTokens, "checkpoint %s", checkpointID)
	require.Equal(t, 725, usage.OutputTokens, "checkpoint %s", checkpointID)
	require.Equal(t, 60603, usage.CacheReadTokens, "checkpoint %s", checkpointID)
}

func TestFactoryDroidWorkerTokensComeFromItsOwnSettings(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	parent := env.NewFactoryDroidSession()
	appendLines(t, parent.TranscriptPath, `{"type":"session_start","id":"`+parent.ID+`"}`)
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmitWithTranscript(parent.ID, parent.TranscriptPath))

	const toolUseID = "toolu_worker_tokens"
	worker := env.NewFactoryDroidSession()
	appendLines(t, worker.TranscriptPath,
		`{"type":"message","id":"w1","message":{"role":"user","content":[{"type":"text","text":"Summarize main.go"}]}}`)
	worker.MarkAsWorkerSession(parent.ID, toolUseID, "worker: Summarize main.go")
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmitWithTranscript(worker.ID, worker.TranscriptPath))
	appendLines(t, worker.TranscriptPath,
		`{"type":"message","id":"w2","message":{"role":"assistant","content":[{"type":"text","text":"It prints hi."}]}}`)
	writeDroidSettings(t, worker.TranscriptPath, 8612, 109, 8026)

	// Twice: a repeated Stop must not count the Worker's turn again.
	require.NoError(t, env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath))
	require.NoError(t, env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath))

	state, err := env.GetSessionState(parent.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec, "the Worker's turn must be a task on the parent")
	require.Equal(t, &agent.TokenUsage{InputTokens: 8612, OutputTokens: 109, CacheReadTokens: 8026, APICallCount: 1}, rec.TokenUsage)

	// The parent's own total excludes the Worker, which counts in the task.
	appendLines(t, parent.TranscriptPath,
		`{"type":"message","id":"p1","message":{"role":"assistant","content":[{"type":"text","text":"The Worker is done."}]}}`)
	writeDroidSettings(t, parent.TranscriptPath, 56099, 1276, 139199)
	require.NoError(t, env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath))
	require.Equal(t, &agent.TokenUsage{InputTokens: 56099, OutputTokens: 1276, CacheReadTokens: 139199, APICallCount: 1},
		sessionTokenTotal(t, env, parent.ID))
}
