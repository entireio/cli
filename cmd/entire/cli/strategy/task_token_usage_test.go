package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// A recount records the model that produced the subagent's tokens, read from
// the transcript's own per-message model.
func TestCondensedTaskTokenUsage_RecordsModel(t *testing.T) {
	t.Parallel()

	withModel := `{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-4-5","usage":{"input_tokens":3,"output_tokens":40}}}` + "\n"
	got := condensedTaskTokenUsage(context.Background(), claudecode.NewClaudeCodeAgent(),
		session.TaskRecord{TokenUsageFromTranscript: true, CompletedAt: time.Now()}, []byte(withModel))
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, OutputTokens: 40, APICallCount: 1, Model: "claude-haiku-4-5"}, got)
}

// TestChildSessionTokens_EachChildCountsOnceAcrossWindows walks two
// checkpoint windows: a foreground child credited during turn 1, then a
// background child credited after turn 2 ended. Each checkpoint stores only
// the child credited in its window, and the session total holds each once.
func TestChildSessionTokens_EachChildCountsOnceAcrossWindows(t *testing.T) {
	t.Parallel()

	turn := &agent.TokenUsage{InputTokens: 100, OutputTokens: 10, APICallCount: 1}
	foreground := &agent.TokenUsage{InputTokens: 150, OutputTokens: 80, CacheReadTokens: 5, CacheCreationTokens: 15, APICallCount: 1}
	background := &agent.TokenUsage{InputTokens: 300, OutputTokens: 160, APICallCount: 2}
	state := &SessionState{SessionID: "s", AgentType: agent.AgentTypeOpenCode}

	applyTurnTokenUsage(state, turn)
	state.AddChildSessionTokens(foreground)
	require.Equal(t, foreground, condensedTokenUsage(t, state).SubagentTokens)
	resetCheckpointWindow(state) // checkpoint 1

	applyTurnTokenUsage(state, turn)
	require.Nil(t, condensedTokenUsage(t, state).SubagentTokens, "turn 2 has no child of its own")
	state.AddChildSessionTokens(background) // the turn ended, then the child went idle
	require.Equal(t, background, condensedTokenUsage(t, state).SubagentTokens,
		"the late child lands in the next checkpoint, and only it")
	resetCheckpointWindow(state) // checkpoint 2

	state.AddChildSessionTokens(nil)
	state.AddChildSessionTokens(&agent.TokenUsage{})
	require.Nil(t, state.PendingChildSessionTokens(), "a child with no usage adds nothing")

	total := state.TotalTokenUsage()
	require.Equal(t, 200, total.InputTokens)
	require.Equal(t, &agent.TokenUsage{InputTokens: 450, OutputTokens: 240, CacheReadTokens: 5, CacheCreationTokens: 15, APICallCount: 3},
		total.SubagentTokens)
}

// TestChildSessionTokens_SurviveSubagentSnapshots credits a Droid Worker and
// then records turns whose usage carries a subagent snapshot, which replaces
// TokenUsage.SubagentTokens. The Worker must still count once, both in the
// session total and in the checkpoint, alongside the snapshot's own window.
func TestChildSessionTokens_SurviveSubagentSnapshots(t *testing.T) {
	t.Parallel()

	worker := &agent.TokenUsage{InputTokens: 8612, OutputTokens: 109, CacheReadTokens: 8026, APICallCount: 1}
	snapshot := func(input int) *agent.TokenUsage {
		return &agent.TokenUsage{
			InputTokens: 10, APICallCount: 1,
			SubagentTokens: &agent.TokenUsage{InputTokens: input, APICallCount: 1},
		}
	}
	state := &SessionState{SessionID: "s", AgentType: agent.AgentTypeFactoryAIDroid}

	applyTurnTokenUsage(state, snapshot(40))
	state.AddChildSessionTokens(worker)
	applyTurnTokenUsage(state, snapshot(70))

	wantSubagents := &agent.TokenUsage{InputTokens: 8682, OutputTokens: 109, CacheReadTokens: 8026, APICallCount: 2}
	require.Equal(t, wantSubagents, state.TotalTokenUsage().SubagentTokens)
	require.Equal(t, wantSubagents, condensedTokenUsage(t, state).SubagentTokens)
	resetCheckpointWindow(state)

	applyTurnTokenUsage(state, snapshot(90))
	require.Equal(t, &agent.TokenUsage{InputTokens: 20, APICallCount: 0}, condensedTokenUsage(t, state).SubagentTokens,
		"the next checkpoint holds the snapshot's growth and not the Worker again")
	require.Equal(t, 8702, state.TotalTokenUsage().SubagentTokens.InputTokens)
}

// condensedTokenUsage is the usage condensation would store for state's
// pending window, for an agent whose transcript recompute found no usage.
func condensedTokenUsage(t *testing.T, state *SessionState) *agent.TokenUsage {
	t.Helper()
	data := &ExtractedSessionData{}
	resolveCondensedTokenUsage(context.Background(), nil, state, data)
	require.NotNil(t, data.TokenUsage)
	return data.TokenUsage
}

// TestCompleteChildSessionTaskRecord_CreditsEachCallOnce checks that a child
// call is credited by the completion that claims its record and never again:
// not by a repeated stop, nor by one arriving after condensation removed the
// record, nor by one with no start. A later call that reuses the call ID with
// a new start is a new call and is credited. A record completed through
// CompleteTaskRecord (Claude Code, whose turns read their subagents) adds none.
func TestCompleteChildSessionTaskRecord_CreditsEachCallOnce(t *testing.T) {
	ctx := context.Background()
	_, state := setupCondensableSessionWithTranscript(t, "2026-10-19-child-session-credit")
	parentUsage := &agent.TokenUsage{InputTokens: 500, OutputTokens: 50, APICallCount: 2}
	state.TokenUsage = parentUsage
	require.NoError(t, SaveSessionState(ctx, state))

	started := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	rec := session.TaskRecord{ToolUseID: "call_child", AgentID: "ses_child", StartedAt: started, TokenUsage: oneCallUsage()}
	stop := func(rec session.TaskRecord, callStartedAt time.Time) {
		t.Helper()
		_, err := CompleteChildSessionTaskRecord(ctx, state.SessionID, rec, callStartedAt)
		require.NoError(t, err)
	}
	condense := func() {
		t.Helper()
		require.NoError(t, MutateSessionState(ctx, state.SessionID, func(state *SessionState) error {
			resetCheckpointWindow(state)
			return nil
		}))
	}

	stop(rec, started)
	stop(rec, started) // a repeated stop
	condense()
	stop(rec, started)     // repeated after condensation removed the record
	stop(rec, time.Time{}) // repeated by a plugin that lost the call's start
	_, err := CompleteTaskRecord(ctx, state.SessionID, session.TaskRecord{ToolUseID: "toolu_claude", AgentID: "aclaude", TokenUsage: oneCallUsage()})
	require.NoError(t, err)

	loaded, err := LoadSessionState(ctx, state.SessionID)
	require.NoError(t, err)
	require.Equal(t, oneCallUsage(), loaded.ChildSessionTokens)
	require.Nil(t, loaded.PendingChildSessionTokens(), "nothing after the first stop is pending")
	require.Equal(t, parentUsage, loaded.TokenUsage, "the parent's own usage is unchanged")

	resumed := rec
	resumed.ToolUseID = "call_child-ses_child-" + strconv.FormatInt(started.Add(time.Minute).UnixMilli(), 10)
	resumed.StartedAt = started.Add(time.Minute)
	stop(resumed, resumed.StartedAt) // a later call that reused the call ID

	loaded, err = LoadSessionState(ctx, state.SessionID)
	require.NoError(t, err)
	require.Equal(t, oneCallUsage(), loaded.PendingChildSessionTokens())
	require.Equal(t, 2, loaded.ChildSessionTokens.APICallCount)
}

// TestCompleteChildSessionTaskRecord_ZeroStartAfterReuseIsSkipped covers a
// call ID reused for the same child, whose stops are keyed apart by start,
// followed by a stop with no start after condensation removed both records.
// The start-less stop cannot be told from a repeat, so it is not credited.
func TestCompleteChildSessionTaskRecord_ZeroStartAfterReuseIsSkipped(t *testing.T) {
	ctx := context.Background()
	_, state := setupCondensableSessionWithTranscript(t, "2026-10-19-child-session-zero-start")
	require.NoError(t, SaveSessionState(ctx, state))

	first := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	for _, rec := range []session.TaskRecord{
		{ToolUseID: "call_1", AgentID: "ses_child", StartedAt: first, TokenUsage: oneCallUsage()},
		{ToolUseID: "call_1-ses_child-" + strconv.FormatInt(second.UnixMilli(), 10), AgentID: "ses_child", StartedAt: second, TokenUsage: oneCallUsage()},
	} {
		_, err := CompleteChildSessionTaskRecord(ctx, state.SessionID, rec, rec.StartedAt)
		require.NoError(t, err)
	}
	require.NoError(t, MutateSessionState(ctx, state.SessionID, func(state *SessionState) error {
		resetCheckpointWindow(state)
		return nil
	}))
	_, err := CompleteChildSessionTaskRecord(ctx, state.SessionID,
		session.TaskRecord{ToolUseID: "call_1", AgentID: "ses_child", StartedAt: time.Now(), TokenUsage: oneCallUsage()}, time.Time{})
	require.NoError(t, err)

	loaded, err := LoadSessionState(ctx, state.SessionID)
	require.NoError(t, err)
	require.Equal(t, 2, loaded.ChildSessionTokens.APICallCount)
	require.Nil(t, loaded.PendingChildSessionTokens())
}

// TestUpsertCompletedTaskRecordTurn_CreditsEachTurnOnce covers a Droid Worker
// that reaches turn-end twice under one parent tool use: the record and the
// parent's child-session total both hold the two turns, each once.
func TestUpsertCompletedTaskRecordTurn_CreditsEachTurnOnce(t *testing.T) {
	ctx := context.Background()
	_, state := setupCondensableSessionWithTranscript(t, "2026-10-19-worker-credit")
	require.NoError(t, SaveSessionState(ctx, state))

	first := &agent.TokenUsage{InputTokens: 8612, OutputTokens: 109, CacheReadTokens: 8026, APICallCount: 1}
	second := &agent.TokenUsage{InputTokens: 100, OutputTokens: 10, APICallCount: 1}
	for _, usage := range []*agent.TokenUsage{first, second} {
		require.NoError(t, UpsertCompletedTaskRecordTurn(ctx, state.SessionID, session.TaskRecord{
			ToolUseID: "tc-worker", AgentID: "worker-session", StartedAt: time.Now(), TokenUsage: usage,
		}))
	}

	loaded, err := LoadSessionState(ctx, state.SessionID)
	require.NoError(t, err)
	both := &agent.TokenUsage{InputTokens: 8712, OutputTokens: 119, CacheReadTokens: 8026, APICallCount: 2}
	require.Equal(t, both, loaded.FindTaskRecord("tc-worker").TokenUsage)
	require.Equal(t, both, loaded.ChildSessionTokens)
	require.Equal(t, both, loaded.PendingChildSessionTokens())
}
