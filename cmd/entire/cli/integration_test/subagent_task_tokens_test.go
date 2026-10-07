//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/stretchr/testify/require"
)

// Subagent transcript lines, shaped like Claude Code 2.1.291's: the agent
// writes its file on its first API call and reports back on its second, which
// is streamed as two rows with the same message id (only the last counts).
const (
	taskTokensFirstCall = `{"type":"user","isSidechain":true,"message":{"role":"user","content":"Write tokens.txt"}}
{"type":"assistant","isSidechain":true,"message":{"id":"msg_sub_1","role":"assistant","content":[{"type":"tool_use","id":"toolu_sub_write","name":"Write","input":{"file_path":"tokens.txt","content":"tokens\n"}}],"usage":{"input_tokens":3,"cache_creation_input_tokens":200,"cache_read_input_tokens":0,"output_tokens":50}}}
`
	taskTokensLastCall = `{"type":"assistant","isSidechain":true,"message":{"id":"msg_sub_2","role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":1,"cache_creation_input_tokens":30,"cache_read_input_tokens":200,"output_tokens":4}}}
{"type":"assistant","isSidechain":true,"message":{"id":"msg_sub_2","role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":1,"cache_creation_input_tokens":30,"cache_read_input_tokens":200,"output_tokens":20}}}
`
)

// TestSubagentTaskTokens_CountCallsWrittenAfterStop is the per-task half of
// #2685. Claude Code fires a subagent's SubagentStop before the agent's last
// API call is in its transcript, so the usage computed at the stop misses it.
// A background agent woken again by a child it launched stops a second time,
// and that stop finds its record already complete. The checkpoint's task.json
// must count every call in the transcript stored beside it.
func TestSubagentTaskTokens_CountCallsWrittenAfterStop(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		rewake   bool
		wantCall int
	}{
		{name: "last call written after the stop", wantCall: 2},
		{name: "agent woken again after its first stop", rewake: true, wantCall: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			sess := env.NewSession()
			sess.CreateTranscript("delegate a background task", nil)
			const (
				toolUseID  = "toolu_01TaskTokens"
				subagentID = "a5555666677778888"
			)

			require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath))
			require.NoError(t, env.SimulatePreTask(sess.ID, sess.TranscriptPath, toolUseID))
			require.NoError(t, env.SimulatePostTask(PostTaskInput{
				SessionID: sess.ID, TranscriptPath: sess.TranscriptPath,
				ToolUseID: toolUseID, AgentID: subagentID, Background: true,
			}))
			require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))

			transcript := filepath.Join(paths.SubagentsDir(filepath.Dir(sess.TranscriptPath), sess.ID),
				paths.AgentTranscriptFileName(subagentID))
			require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o750))
			require.NoError(t, os.WriteFile(transcript, []byte(taskTokensFirstCall), 0o600))
			env.WriteFile("tokens.txt", "tokens\n")
			stop := SubagentStopInput{
				SessionID: sess.ID, TranscriptPath: sess.TranscriptPath,
				AgentID: subagentID, AgentTranscriptPath: transcript,
			}
			require.NoError(t, env.SimulateSubagentStop(stop))

			content := taskTokensFirstCall + taskTokensLastCall
			if tt.rewake {
				content += `{"type":"assistant","isSidechain":true,"message":{"id":"msg_sub_3","role":"assistant","content":[{"type":"text","text":"Child finished."}],"usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":230,"output_tokens":6}}}` + "\n"
			}
			require.NoError(t, os.WriteFile(transcript, []byte(content), 0o600))
			if tt.rewake {
				require.NoError(t, env.SimulateSubagentStop(stop))
			}

			env.GitCommitWithShadowHooksAsAgent("Add tokens file", "tokens.txt")
			checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
			require.NotEmpty(t, checkpointID)

			raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID, "task.json"))
			require.True(t, ok, "task.json not materialized")
			var task struct {
				Files      []string `json:"files"`
				TokenUsage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					APICallCount int `json:"api_call_count"`
				} `json:"token_usage"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &task))
			require.Equal(t, []string{"tokens.txt"}, task.Files, raw)
			require.NotNil(t, task.TokenUsage, raw)
			require.Equal(t, tt.wantCall, task.TokenUsage.APICallCount, raw)
			wantOutput := 50 + 20
			if tt.rewake {
				wantOutput += 6
			}
			require.Equal(t, wantOutput, task.TokenUsage.OutputTokens, raw)

			stored, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
				CheckpointTaskFilePath(checkpointID, toolUseID, paths.AgentTranscriptFileName(subagentID)))
			require.True(t, ok, "subagent transcript not materialized")
			require.Contains(t, stored, "msg_sub_2", "the stored transcript holds the call task.json counts")
		})
	}
}
