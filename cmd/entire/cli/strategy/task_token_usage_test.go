package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/stretchr/testify/require"
)

// subagentTranscriptOneCall is a Claude Code subagent transcript as a stop
// that fired early read it: one API call. subagentTranscriptTwoCalls is the
// same transcript once the agent wrote its last call, streamed as two rows
// with the same message id, of which only the final one counts.
const (
	subagentTranscriptOneCall = `{"type":"user","message":{"role":"user","content":"write a.txt"}}
{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"output_tokens":40}}}
`
	subagentTranscriptTwoCalls = subagentTranscriptOneCall +
		`{"type":"assistant","message":{"id":"msg_2","usage":{"input_tokens":1,"cache_creation_input_tokens":20,"cache_read_input_tokens":100,"output_tokens":5}}}
{"type":"assistant","message":{"id":"msg_2","usage":{"input_tokens":1,"cache_creation_input_tokens":20,"cache_read_input_tokens":100,"output_tokens":60}}}
`
)

func oneCallUsage() *agent.TokenUsage {
	return &agent.TokenUsage{InputTokens: 3, CacheCreationTokens: 100, OutputTokens: 40, APICallCount: 1}
}

func TestCondensedTaskTokenUsage(t *testing.T) {
	t.Parallel()

	both := &agent.TokenUsage{InputTokens: 4, CacheCreationTokens: 120, CacheReadTokens: 100, OutputTokens: 100, APICallCount: 2}
	completed := time.Now()
	manyCalls := &agent.TokenUsage{InputTokens: 50, OutputTokens: 50, APICallCount: 5}

	tests := []struct {
		name       string
		ag         agent.Agent
		record     session.TaskRecord
		transcript string
		want       *agent.TokenUsage
	}{
		{
			name:       "usage from the transcript is recounted from the stored transcript",
			ag:         claudecode.NewClaudeCodeAgent(),
			record:     session.TaskRecord{TokenUsage: oneCallUsage(), TokenUsageFromTranscript: true, CompletedAt: completed},
			transcript: subagentTranscriptTwoCalls,
			want:       both,
		},
		{
			name:       "a completion read that found no usage is recounted",
			ag:         claudecode.NewClaudeCodeAgent(),
			record:     session.TaskRecord{TokenUsageFromTranscript: true, CompletedAt: completed},
			transcript: subagentTranscriptTwoCalls,
			want:       both,
		},
		{
			name:       "usage the agent reported is kept",
			ag:         claudecode.NewClaudeCodeAgent(),
			record:     session.TaskRecord{TokenUsage: oneCallUsage(), CompletedAt: completed},
			transcript: subagentTranscriptTwoCalls,
			want:       oneCallUsage(),
		},
		{
			name:       "a live record is not counted until it completes",
			ag:         claudecode.NewClaudeCodeAgent(),
			record:     session.TaskRecord{TokenUsageFromTranscript: true},
			transcript: subagentTranscriptTwoCalls,
			want:       nil,
		},
		{
			name:       "a recount below the recorded calls means a different file and is ignored",
			ag:         claudecode.NewClaudeCodeAgent(),
			record:     session.TaskRecord{TokenUsage: manyCalls, TokenUsageFromTranscript: true, CompletedAt: completed},
			transcript: subagentTranscriptTwoCalls,
			want:       manyCalls,
		},
		{
			name:       "a transcript without usage keeps the recorded usage",
			ag:         claudecode.NewClaudeCodeAgent(),
			record:     session.TaskRecord{TokenUsage: oneCallUsage(), TokenUsageFromTranscript: true, CompletedAt: completed},
			transcript: `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n",
			want:       oneCallUsage(),
		},
		{
			name:       "an unknown agent keeps the recorded usage",
			record:     session.TaskRecord{TokenUsage: oneCallUsage(), TokenUsageFromTranscript: true, CompletedAt: completed},
			transcript: subagentTranscriptTwoCalls,
			want:       oneCallUsage(),
		},
		{
			// Codex sets usage from its rollout inventory, where nil is
			// deliberate for a forked child carrying the parent's history.
			name:       "a Codex record keeps its nil usage",
			ag:         &codex.CodexAgent{},
			record:     session.TaskRecord{CompletedAt: completed},
			transcript: `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":9000,"output_tokens":9000}}}}` + "\n",
			want:       nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := condensedTaskTokenUsage(context.Background(), tt.ag, tt.record, []byte(tt.transcript))
			require.Equal(t, tt.want, got)
		})
	}
}

// TestMaterializeTaskRecords_RecountsTokensFromStoredTranscript drives a
// completion through CompleteTaskRecord, the path a SubagentStop takes, while
// the transcript holds one API call, then lets the agent write its last call
// before condensation. The stored task must count both, matching the transcript
// stored beside it.
func TestMaterializeTaskRecords_RecountsTokensFromStoredTranscript(t *testing.T) {
	const toolUseID, agentID = "toolu_tokens", "atokens01"
	_, state := setupCondensableSessionWithTranscript(t, "2026-10-07-task-token-recount")
	require.NoError(t, SaveSessionState(context.Background(), state))

	transcriptPath := filepath.Join(t.TempDir(), "agent-"+agentID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(subagentTranscriptOneCall), 0o600))

	completedNow, err := CompleteTaskRecord(context.Background(), state.SessionID, session.TaskRecord{
		ToolUseID:                toolUseID,
		AgentID:                  agentID,
		StartedAt:                time.Now(),
		DeclaredTranscriptPath:   transcriptPath,
		TokenUsage:               oneCallUsage(),
		TokenUsageFromTranscript: true,
	})
	require.NoError(t, err)
	require.True(t, completedNow)

	require.NoError(t, os.WriteFile(transcriptPath, []byte(subagentTranscriptTwoCalls), 0o600))
	state, err = LoadSessionState(context.Background(), state.SessionID)
	require.NoError(t, err)
	require.True(t, state.FindTaskRecord(toolUseID).TokenUsageFromTranscript, "completion must keep where the usage came from")

	payloads, _ := (&ManualCommitStrategy{}).materializeTaskRecords(context.Background(), context.Background(),
		claudecode.NewClaudeCodeAgent(), state, nil)
	require.Len(t, payloads, 1)
	require.NotNil(t, payloads[0].TokenUsage)
	require.Equal(t, 2, payloads[0].TokenUsage.APICallCount)
	require.Equal(t, 100, payloads[0].TokenUsage.OutputTokens)
	require.Contains(t, string(payloads[0].Transcript.Bytes()), "msg_2")
}
