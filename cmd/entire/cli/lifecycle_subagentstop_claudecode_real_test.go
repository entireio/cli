package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// --- GitHub issue #2215 ---
//
// Claude Code's genuine SubagentStop hook payload never carries tool_use_id
// (Claude Code's documented hooks contract lists that field on tool events
// only — PreToolUse/PostToolUse/PostToolUseFailure). Entire's correlation
// used to key exclusively on ToolUseID, so a real SubagentStop always missed
// its launch marker and the subagent's work was silently dropped: no task
// record, no files, no tokens, no transcript.
//
// These tests drive the REAL production pipeline end to end — the actual
// claudecode.ClaudeCodeAgent's ParseHookEvent (not a hand-built agent.Event)
// parsing JSON payloads whose shape matches Claude Code's real wire format
// byte-for-byte (verified against the key set the issue's live-payload
// capture reported: agent_id, agent_type, cwd, hook_event_name,
// last_assistant_message, permission_mode, session_id, stop_hook_active,
// transcript_path — no tool_use_id, no agent_transcript_path), then the real
// DispatchLifecycleEvent dispatcher — never a hand-mocked call into a lower
// layer.

// realClaudeCodePostTaskPayload builds the JSON PostToolUse[Agent]
// ("post-task") payload Claude Code sends when a background Task launch is
// acknowledged. This is the ONLY point at which Entire ever learns the
// (tool_use_id, agent_id) pairing for a background subagent — SubagentStop
// never repeats tool_use_id.
func realClaudeCodePostTaskPayload(sessionID, transcriptPath, toolUseID, agentID, label string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"transcript_path":%q,"hook_event_name":"PostToolUse","tool_name":"Agent",`+
			`"tool_use_id":%q,"tool_input":{"description":"repro-%s","prompt":"look","subagent_type":"Explore","run_in_background":true},`+
			`"tool_response":{"agentId":%q}}`,
		sessionID, transcriptPath, toolUseID, label, agentID,
	)
}

// realClaudeCodeSubagentStopPayload builds the JSON SubagentStop payload
// exactly as Claude Code actually sends it — reproduced verbatim from GitHub
// issue #2215's live-payload capture (its Debug key-log dump listed exactly
// these keys and no others). Deliberately carries NO tool_use_id and NO
// agent_transcript_path.
func realClaudeCodeSubagentStopPayload(sessionID, transcriptPath, cwd, agentID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"transcript_path":%q,"cwd":%q,"permission_mode":"default",`+
			`"hook_event_name":"SubagentStop","agent_id":%q,"agent_type":"Explore",`+
			`"last_assistant_message":"done","stop_hook_active":false}`,
		sessionID, transcriptPath, cwd, agentID,
	)
}

// TestHandleLifecycleSubagentEnd_ClaudeCodeRealSubagentStopPayload_CorrelatesViaAgentID
// is the single-subagent reproduction: a real background Task launch,
// followed by Claude Code's real (tool_use_id-less) SubagentStop payload,
// driven through claudecode.ClaudeCodeAgent.ParseHookEvent and
// DispatchLifecycleEvent — the actual production parse+dispatch path, not a
// hand-built agent.Event.
//
// Run against the unfixed code, this test fails: the SubagentStop event
// cannot find its launch marker (ToolUseID-only lookup, empty ToolUseID),
// handleSubagentStopFinal returns early, and the task record is never
// completed. After the fix (AgentID fallback correlation in
// handleSubagentStopFinal / session.State.FindTaskRecordByAgentID), it
// passes.
func TestHandleLifecycleSubagentEnd_ClaudeCodeRealSubagentStopPayload_CorrelatesViaAgentID(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "claudecode-real-payload-session"
	toolUseID := "toolu_01ReproControlAAAAAAAA"
	agentID := "a1b2c3d4e5f60718"

	transcriptDir := t.TempDir()
	mainTranscriptPath := filepath.Join(transcriptDir, "main.jsonl")
	require.NoError(t, os.WriteFile(mainTranscriptPath,
		[]byte(`{"type":"human","message":{"content":"go look into this"}}`+"\n"), 0o600))

	// The subagent's own transcript, at the layout Claude Code actually
	// writes it to (ResolveAgentTranscriptPath's primary layout). The real
	// SubagentStop payload carries no agent_transcript_path, so this is the
	// only way the completed record can ever find it.
	const subagentSentinel = "explored the repo; verdict: sentinel-single"
	subagentsDir := paths.SubagentsDir(filepath.Dir(mainTranscriptPath), sessionID)
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))
	subagentTranscriptPath := filepath.Join(subagentsDir, paths.AgentTranscriptFileName(agentID))
	require.NoError(t, os.WriteFile(subagentTranscriptPath,
		[]byte(`{"type":"assistant","message":{"content":"`+subagentSentinel+`"}}`+"\n"), 0o600))

	saveInFlightTranscriptSession(ctx, t, sessionID, headHash, session.PhaseActive, mainTranscriptPath)

	ag := &claudecode.ClaudeCodeAgent{}

	// Step 1: the real background-launch acknowledgement. Records the
	// in-flight marker carrying both toolUseID and agentID.
	postTaskEvent, err := ag.ParseHookEvent(ctx, claudecode.HookNamePostTask,
		strings.NewReader(realClaudeCodePostTaskPayload(sessionID, mainTranscriptPath, toolUseID, agentID, "single")))
	require.NoError(t, err)
	require.NotNil(t, postTaskEvent)
	require.NoError(t, DispatchLifecycleEvent(ctx, ag, postTaskEvent))

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	require.Len(t, state.TaskRecords, 1, "background launch must record an in-flight marker")
	assert.Equal(t, toolUseID, state.TaskRecords[0].ToolUseID)
	assert.Equal(t, agentID, state.TaskRecords[0].AgentID)

	// Step 2: the real SubagentStop. Parse through the actual production
	// parser and confirm — as evidence the fixture really is realistic — that
	// the parsed event carries no ToolUseID, only SubagentID.
	subagentStopEvent, err := ag.ParseHookEvent(ctx, claudecode.HookNameSubagentStop,
		strings.NewReader(realClaudeCodeSubagentStopPayload(sessionID, mainTranscriptPath, repoDir, agentID)))
	require.NoError(t, err)
	require.NotNil(t, subagentStopEvent)
	require.Empty(t, subagentStopEvent.ToolUseID,
		"a real Claude Code SubagentStop payload carries no tool_use_id — this is the bug's precondition")
	require.Equal(t, agentID, subagentStopEvent.SubagentID)
	require.True(t, subagentStopEvent.Final)

	require.NoError(t, DispatchLifecycleEvent(ctx, ag, subagentStopEvent))

	state, loadErr = strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec, "the marker recorded at launch must still be found by its original tool_use_id")
	assert.False(t, rec.CompletedAt.IsZero(),
		"a real Claude Code SubagentStop (agent_id only, no tool_use_id) must correlate to its launch marker and complete the record")
	assert.Equal(t, subagentTranscriptPath, rec.DeclaredTranscriptPath,
		"the completed record must resolve to this subagent's own transcript, via the marker's AgentID")
	assert.Empty(t, state.LiveTaskRecords(), "the record must no longer be in flight")
}

// claudeCodeMultiSubagent groups one background subagent's identifiers and
// its on-disk transcript for the disambiguation test below.
type claudeCodeMultiSubagent struct {
	label          string
	toolUseID      string
	agentID        string
	transcriptPath string
}

// TestHandleLifecycleSubagentEnd_ClaudeCodeRealSubagentStopPayload_MultipleSubagentsDisambiguate
// covers three subagents dispatched in one session — the case the issue
// specifically calls out (nested/skill-fanned-out subagents), reproduced
// here as three independent background launches followed by their
// SubagentStop events arriving out of launch order. Every SubagentStop
// payload again carries only its own agent_id, never tool_use_id. This pins
// that the AgentID correlation fix disambiguates correctly — each stop must
// complete ONLY its own record, using ITS OWN transcript path — rather than,
// say, always claiming the first/last live marker or cross-wiring a
// transcript from a different subagent.
func TestHandleLifecycleSubagentEnd_ClaudeCodeRealSubagentStopPayload_MultipleSubagentsDisambiguate(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "claudecode-real-payload-multi-session"

	transcriptDir := t.TempDir()
	mainTranscriptPath := filepath.Join(transcriptDir, "main.jsonl")
	require.NoError(t, os.WriteFile(mainTranscriptPath,
		[]byte(`{"type":"human","message":{"content":"fan out three subagents"}}`+"\n"), 0o600))
	subagentsDir := paths.SubagentsDir(filepath.Dir(mainTranscriptPath), sessionID)
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))

	saveInFlightTranscriptSession(ctx, t, sessionID, headHash, session.PhaseActive, mainTranscriptPath)

	ag := &claudecode.ClaudeCodeAgent{}

	subagents := []claudeCodeMultiSubagent{
		{label: "A", toolUseID: "toolu_A", agentID: "agent-nested-A"},
		{label: "B", toolUseID: "toolu_B", agentID: "agent-nested-B"},
		{label: "C", toolUseID: "toolu_C", agentID: "agent-nested-C"},
	}

	// Launch all three background subagents sequentially (as a real session
	// fanning out several Task calls over one turn, or a skill dispatching
	// several at once, would produce). Each records its own in-flight marker.
	for i := range subagents {
		sa := &subagents[i]
		sa.transcriptPath = filepath.Join(subagentsDir, paths.AgentTranscriptFileName(sa.agentID))
		require.NoError(t, os.WriteFile(sa.transcriptPath,
			[]byte(`{"type":"assistant","message":{"content":"verdict-`+sa.label+`"}}`+"\n"), 0o600))

		ev, err := ag.ParseHookEvent(ctx, claudecode.HookNamePostTask,
			strings.NewReader(realClaudeCodePostTaskPayload(sessionID, mainTranscriptPath, sa.toolUseID, sa.agentID, sa.label)))
		require.NoError(t, err)
		require.NoError(t, DispatchLifecycleEvent(ctx, ag, ev))
	}

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.Len(t, state.TaskRecords, 3, "all three background launches must record distinct in-flight markers")
	require.Len(t, state.LiveTaskRecords(), 3)

	// Fire SubagentStop in a DIFFERENT order than launch (B, then C, then A).
	// After each one, check every subagent's record: the ones stopped so far
	// must be completed with THEIR OWN transcript path; the ones not yet
	// stopped must remain untouched and live.
	stopOrder := []int{1, 2, 0} // B, C, A
	stopped := map[string]bool{}

	for _, idx := range stopOrder {
		sa := subagents[idx]

		ev, err := ag.ParseHookEvent(ctx, claudecode.HookNameSubagentStop,
			strings.NewReader(realClaudeCodeSubagentStopPayload(sessionID, mainTranscriptPath, repoDir, sa.agentID)))
		require.NoError(t, err)
		require.Empty(t, ev.ToolUseID, "subagent %s's real SubagentStop payload must carry no tool_use_id", sa.label)
		require.Equal(t, sa.agentID, ev.SubagentID)

		require.NoError(t, DispatchLifecycleEvent(ctx, ag, ev))
		stopped[sa.agentID] = true

		st, loadErr := strategy.LoadSessionState(ctx, sessionID)
		require.NoError(t, loadErr)

		for _, other := range subagents {
			rec := st.FindTaskRecord(other.toolUseID)
			require.NotNil(t, rec, "subagent %s's marker must never disappear", other.label)
			if stopped[other.agentID] {
				assert.False(t, rec.CompletedAt.IsZero(),
					"subagent %s must be completed once its own SubagentStop has fired", other.label)
				assert.Equal(t, other.transcriptPath, rec.DeclaredTranscriptPath,
					"subagent %s must resolve to ITS OWN transcript, not another subagent's", other.label)
			} else {
				assert.True(t, rec.CompletedAt.IsZero(),
					"subagent %s must remain live — its own SubagentStop has not fired yet", other.label)
			}
		}
	}

	// Final cross-check: all three completed, each with a distinct,
	// correctly-attributed transcript path — no cross-wiring anywhere.
	finalState, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	assert.Empty(t, finalState.LiveTaskRecords(), "every subagent must be completed by the end")

	seenTranscripts := map[string]string{} // transcript path -> label that claimed it
	for _, sa := range subagents {
		rec := finalState.FindTaskRecord(sa.toolUseID)
		require.NotNil(t, rec)
		assert.False(t, rec.CompletedAt.IsZero())
		assert.Equal(t, sa.transcriptPath, rec.DeclaredTranscriptPath)
		if owner, seen := seenTranscripts[rec.DeclaredTranscriptPath]; seen {
			t.Errorf("transcript %q attributed to both %q and %q — disambiguation failed", rec.DeclaredTranscriptPath, owner, sa.label)
		}
		seenTranscripts[rec.DeclaredTranscriptPath] = sa.label
	}
}
