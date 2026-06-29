package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

const (
	childID1 = "019e84ed-497d-7511-941f-7a01260d5136"
	childID2 = "019e84f6-db7b-7380-8ab6-9fcb551901e3"
	childID3 = "019e8502-0220-7c62-ad81-c967a8bc5526" // referenced but no rollout written
)

// rolloutLineJSON builds one rollout JSONL line: {"type":..,"payload":..}.
func rolloutLineJSON(t *testing.T, typ string, payload map[string]any) string {
	t.Helper()
	pb, err := json.Marshal(payload)
	require.NoError(t, err)
	lb, err := json.Marshal(map[string]any{"type": typ, "payload": json.RawMessage(pb)})
	require.NoError(t, err)
	return string(lb)
}

func applyPatchLine(t *testing.T, verb, path string) string {
	t.Helper()
	input := "*** Begin Patch\n*** " + verb + " File: " + path + "\n*** End Patch"
	return rolloutLineJSON(t, "response_item", map[string]any{
		"type": "custom_tool_call", "name": "apply_patch", "input": input,
	})
}

func functionCallLine(t *testing.T, name, arguments string) string {
	t.Helper()
	return rolloutLineJSON(t, "response_item", map[string]any{
		"type": "function_call", "name": name, "arguments": arguments, "call_id": "call_x",
	})
}

func tokenCountLine(t *testing.T, input, cached, output int) string {
	t.Helper()
	return rolloutLineJSON(t, "event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage": map[string]any{
				"input_tokens":        input,
				"cached_input_tokens": cached,
				"output_tokens":       output,
				"total_tokens":        input + output,
			},
		},
	})
}

func sessionMetaLine(t *testing.T, id string) string {
	t.Helper()
	return rolloutLineJSON(t, "session_meta", map[string]any{
		"id": id, "timestamp": "2026-06-29T12:00:00.000Z",
	})
}

// writeChildRollout writes a flat child rollout file discoverable by
// findRolloutBySessionID under the test sessions dir.
func writeChildRollout(t *testing.T, sessionsDir, id string, lines ...string) {
	t.Helper()
	path := filepath.Join(sessionsDir, "rollout-2026-06-29T12-00-00-"+id+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// --- ParseHookEvent: subagent hooks ---

func TestParseHookEvent_SubagentStart(t *testing.T) {
	ag := &CodexAgent{}
	in := `{"session_id":"parent-sess","transcript_path":"/tmp/parent.jsonl","agent_id":"` + childID1 +
		`","agent_type":"explorer","turn_id":"turn-1","model":"gpt-5.5"}`

	evt, err := ag.ParseHookEvent(t.Context(), HookNameSubagentStart, strings.NewReader(in))
	require.NoError(t, err)
	require.NotNil(t, evt)
	require.Equal(t, agent.SubagentStart, evt.Type)
	require.Equal(t, "parent-sess", evt.SessionID)
	require.Equal(t, "/tmp/parent.jsonl", evt.SessionRef)
	require.Equal(t, childID1, evt.ToolUseID)
	require.Equal(t, childID1, evt.SubagentID)
	require.Equal(t, "explorer", evt.SubagentType)
}

func TestParseHookEvent_SubagentStop(t *testing.T) {
	ag := &CodexAgent{}
	in := `{"session_id":"parent-sess","transcript_path":"/tmp/parent.jsonl","agent_id":"` + childID1 +
		`","agent_type":"explorer","agent_transcript_path":"/tmp/child.jsonl","last_assistant_message":"done"}`

	evt, err := ag.ParseHookEvent(t.Context(), HookNameSubagentStop, strings.NewReader(in))
	require.NoError(t, err)
	require.NotNil(t, evt)
	require.Equal(t, agent.SubagentEnd, evt.Type)
	require.Equal(t, childID1, evt.SubagentID)
	require.Equal(t, "explorer", evt.SubagentType)
	require.Equal(t, "/tmp/child.jsonl", evt.Metadata[metaKeyAgentTranscriptPath])
}

func TestParseHookEvent_SubagentHooks_ErrorOnEmpty(t *testing.T) {
	ag := &CodexAgent{}
	for _, hook := range []string{HookNameSubagentStart, HookNameSubagentStop} {
		_, err := ag.ParseHookEvent(t.Context(), hook, strings.NewReader(""))
		require.Error(t, err, hook)
	}
}

// --- ResolveSubagentTranscript ---

func TestResolveSubagentTranscript_PrefersHookPath(t *testing.T) {
	ag := &CodexAgent{}
	evt := &agent.Event{SubagentID: childID1, Metadata: map[string]string{metaKeyAgentTranscriptPath: "/tmp/from-hook.jsonl"}}
	require.Equal(t, "/tmp/from-hook.jsonl", ag.ResolveSubagentTranscript(evt))
}

func TestResolveSubagentTranscript_GlobsSessionsByAgentID(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)
	writeChildRollout(t, sessionsDir, childID1, sessionMetaLine(t, childID1))

	ag := &CodexAgent{}
	got := ag.ResolveSubagentTranscript(&agent.Event{SubagentID: childID1})
	require.NotEmpty(t, got)
	require.Contains(t, got, childID1)
}

func TestResolveSubagentTranscript_NilAndEmpty(t *testing.T) {
	ag := &CodexAgent{}
	require.Empty(t, ag.ResolveSubagentTranscript(nil))
	require.Empty(t, ag.ResolveSubagentTranscript(&agent.Event{}))
}

// --- extractSpawnedAgentIDs ---

func TestExtractSpawnedAgentIDs(t *testing.T) {
	parent := strings.Join([]string{
		sessionMetaLine(t, "parent"),
		functionCallLine(t, "spawn_agent", `{"agent_type":"explorer","message":"go look"}`),
		functionCallLine(t, "wait_agent", `{"targets":["`+childID1+`","`+childID2+`"],"timeout_ms":1000}`),
		functionCallLine(t, "close_agent", `{"target":"`+childID1+`"}`),
	}, "\n")

	ids := extractSpawnedAgentIDs([]byte(parent), 0)
	require.ElementsMatch(t, []string{childID1, childID2}, ids)
}

// fromOffset scopes subagent discovery to the current checkpoint range. A
// management call before the offset (a prior turn, since a Codex rollout grows
// in one file) must NOT re-attribute its subagent's files or tokens — otherwise
// every prior subagent is re-counted on every subsequent turn.
func TestSubagentDiscovery_RespectsFromOffset(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)
	writeChildRollout(t, sessionsDir, childID1,
		sessionMetaLine(t, childID1), applyPatchLine(t, "Add", "child1.go"), tokenCountLine(t, 100, 0, 50))

	parent := strings.Join([]string{
		sessionMetaLine(t, "parent"),                                      // line 1
		functionCallLine(t, "wait_agent", `{"targets":["`+childID1+`"]}`), // line 2 (prior range)
		applyPatchLine(t, "Update", "parent-late.go"),                     // line 3 (this range)
	}, "\n")

	ag := &CodexAgent{}
	// Offset past the wait_agent line: childID1 belongs to a prior checkpoint.
	files, err := ag.ExtractAllModifiedFiles([]byte(parent), 2, "")
	require.NoError(t, err)
	require.Equal(t, []string{"parent-late.go"}, files, "subagent files before the offset must not be re-attributed")

	usage, err := ag.CalculateTotalTokenUsage([]byte(parent), 2, "")
	require.NoError(t, err)
	if usage != nil {
		require.Nil(t, usage.SubagentTokens, "subagent tokens before the offset must not be re-attributed")
	}

	// Sanity: with offset 0 the subagent IS attributed.
	files0, err := (&CodexAgent{}).ExtractAllModifiedFiles([]byte(parent), 0, "")
	require.NoError(t, err)
	require.Contains(t, files0, "child1.go")
}

// --- SubagentAwareExtractor: files + tokens ---

func TestExtractAllModifiedFiles_IncludesSubagents(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)

	writeChildRollout(t, sessionsDir, childID1, applyPatchLine(t, "Add", "child1.go"))
	writeChildRollout(t, sessionsDir, childID2, applyPatchLine(t, "Update", "child2.go"))
	// childID3 is referenced but has no rollout file → must be skipped gracefully.

	parent := strings.Join([]string{
		sessionMetaLine(t, "parent"),
		applyPatchLine(t, "Update", "parent.go"),
		functionCallLine(t, "wait_agent", `{"targets":["`+childID1+`","`+childID2+`","`+childID3+`"]}`),
	}, "\n")

	ag := &CodexAgent{}
	files, err := ag.ExtractAllModifiedFiles([]byte(parent), 0, "")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"parent.go", "child1.go", "child2.go"}, files)
}

func TestCalculateTotalTokenUsage_AggregatesSubagents(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)

	writeChildRollout(t, sessionsDir, childID1, sessionMetaLine(t, childID1), tokenCountLine(t, 100, 20, 50))
	writeChildRollout(t, sessionsDir, childID2, sessionMetaLine(t, childID2), tokenCountLine(t, 200, 0, 40))

	parent := strings.Join([]string{
		sessionMetaLine(t, "parent"),
		tokenCountLine(t, 10, 0, 5),
		functionCallLine(t, "wait_agent", `{"targets":["`+childID1+`","`+childID2+`"]}`),
	}, "\n")

	ag := &CodexAgent{}
	usage, err := ag.CalculateTotalTokenUsage([]byte(parent), 0, "")
	require.NoError(t, err)
	require.NotNil(t, usage)

	// Main: input 10 (no cache), output 5, 1 call.
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 1, usage.APICallCount)

	// Subagents: child1 fresh=80 cacheRead=20 out=50; child2 fresh=200 out=40.
	require.NotNil(t, usage.SubagentTokens)
	require.Equal(t, 280, usage.SubagentTokens.InputTokens)
	require.Equal(t, 20, usage.SubagentTokens.CacheReadTokens)
	require.Equal(t, 90, usage.SubagentTokens.OutputTokens)
	require.Equal(t, 2, usage.SubagentTokens.APICallCount)
}

func TestCalculateTotalTokenUsage_NoSubagents(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)

	parent := strings.Join([]string{
		sessionMetaLine(t, "parent"),
		tokenCountLine(t, 10, 0, 5),
	}, "\n")

	ag := &CodexAgent{}
	usage, err := ag.CalculateTotalTokenUsage([]byte(parent), 0, "")
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Nil(t, usage.SubagentTokens, "no SubagentTokens when no subagents were spawned")
}
