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
	childID3 = "019e8502-0220-7c62-ad81-c967a8bc5526" // spawned but no rollout written
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

// spawnAgentLines emits a spawn_agent function_call and its matching
// function_call_output, mirroring real Codex rollouts. The output field is a JSON
// *string* whose contents are JSON ("output":"{\"agent_id\":...}") — the real
// Codex wire format — carrying the new child's agent_id, which discovery keys off.
func spawnAgentLines(t *testing.T, callID, agentType, childID string) []string {
	t.Helper()
	call := rolloutLineJSON(t, "response_item", map[string]any{
		"type": "function_call", "name": "spawn_agent",
		"arguments": `{"agent_type":"` + agentType + `","message":"go"}`, "call_id": callID,
	})
	out := rolloutLineJSON(t, "response_item", map[string]any{
		"type": "function_call_output", "call_id": callID,
		"output": `{"agent_id":"` + childID + `","nickname":"n"}`, // STRING, matching real Codex
	})
	return []string{call, out}
}

// agentIDFromSpawnOutput must handle the real Codex string form, the object form,
// and reject non-agent outputs.
func TestAgentIDFromSpawnOutput(t *testing.T) {
	// Real Codex: output is a JSON string containing JSON.
	stringForm, err := json.Marshal(`{"agent_id":"` + childID1 + `","nickname":"Raman"}`)
	require.NoError(t, err)
	require.Equal(t, childID1, agentIDFromSpawnOutput(stringForm))

	// Defensive: object form.
	objForm, err := json.Marshal(map[string]any{"agent_id": childID2, "nickname": "Bohr"})
	require.NoError(t, err)
	require.Equal(t, childID2, agentIDFromSpawnOutput(objForm))

	// A shell tool's plain-text string output yields no id.
	shellForm, err := json.Marshal("Command: cat > hello.txt\nDone")
	require.NoError(t, err)
	require.Empty(t, agentIDFromSpawnOutput(shellForm))
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

// sessionMetaLineWithSource builds a session_meta line with an explicit
// thread_source ("user" or "subagent").
func sessionMetaLineWithSource(t *testing.T, id, threadSource string) string {
	t.Helper()
	return rolloutLineJSON(t, "session_meta", map[string]any{
		"id": id, "timestamp": "2026-06-29T12:00:00.000Z", "thread_source": threadSource,
	})
}

// A subagent's own turn fires UserPromptSubmit/Stop tagged with the PARENT
// session_id but the CHILD (thread_source="subagent") transcript. Those must NOT
// become parent TurnStart/TurnEnd events — otherwise the subagent's task prompt
// overwrites the parent's prompt in `entire status` and churns its phase. A
// real user session (thread_source="user") still produces the events.
func TestParseHookEvent_SkipsSubagentOwnTurns(t *testing.T) {
	dir := t.TempDir()
	write := func(name, threadSource, prompt string) string {
		p := filepath.Join(dir, name)
		lines := []string{
			sessionMetaLineWithSource(t, "id-"+name, threadSource),
			rolloutLineJSON(t, "response_item", map[string]any{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": prompt}},
			}),
		}
		require.NoError(t, os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
		return p
	}
	childPath := write("child.jsonl", "subagent", "Working directory: /x Task: create the file")
	parentPath := write("parent.jsonl", "user", "Refactor the auth module")

	ag := &CodexAgent{}
	hook := func(transcript string) string {
		return `{"session_id":"019f1664-4db6-71d2-9092-a3436b96fb4d","transcript_path":"` + transcript + `","prompt":"p"}`
	}

	// Subagent transcript → both turn hooks are skipped (nil event).
	for _, h := range []string{HookNameUserPromptSubmit, HookNameStop} {
		evt, err := ag.ParseHookEvent(t.Context(), h, strings.NewReader(hook(childPath)))
		require.NoError(t, err)
		require.Nil(t, evt, "subagent-transcript turn must be skipped for hook %s", h)
	}

	// Parent (user) transcript → normal TurnStart / TurnEnd.
	start, err := ag.ParseHookEvent(t.Context(), HookNameUserPromptSubmit, strings.NewReader(hook(parentPath)))
	require.NoError(t, err)
	require.NotNil(t, start)
	require.Equal(t, agent.TurnStart, start.Type)

	end, err := ag.ParseHookEvent(t.Context(), HookNameStop, strings.NewReader(hook(parentPath)))
	require.NoError(t, err)
	require.NotNil(t, end)
	require.Equal(t, agent.TurnEnd, end.Type)
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
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "child.jsonl")
	require.NoError(t, os.WriteFile(hookPath, []byte("{}\n"), 0o600))

	ag := &CodexAgent{}
	evt := &agent.Event{SubagentID: childID1, Metadata: map[string]string{metaKeyAgentTranscriptPath: hookPath}}
	require.Equal(t, hookPath, ag.ResolveSubagentTranscript(evt))
}

// A stale/moved hook path must not shadow the glob fallback — resolution degrades
// to globbing the sessions tree by agent_id rather than returning a dead path.
func TestResolveSubagentTranscript_StaleHookPathFallsBackToGlob(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)
	writeChildRollout(t, sessionsDir, childID1, sessionMetaLine(t, childID1))

	ag := &CodexAgent{}
	evt := &agent.Event{
		SubagentID: childID1,
		Metadata:   map[string]string{metaKeyAgentTranscriptPath: "/nonexistent/stale.jsonl"},
	}
	got := ag.ResolveSubagentTranscript(evt)
	require.NotEmpty(t, got, "stale hook path should fall back to glob")
	require.Contains(t, got, childID1)
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

// Discovery keys off spawn_agent OUTPUTS (which carry the new child id), not
// other agent-tool calls — so a child surfaced only by a non-spawn tool is
// excluded, preventing the resume_agent cross-turn double-count.
func TestExtractSpawnedAgentIDs(t *testing.T) {
	lines := []string{sessionMetaLine(t, "parent")}
	lines = append(lines, spawnAgentLines(t, "call_1", "explorer", childID1)...)
	lines = append(lines, spawnAgentLines(t, "call_2", "explorer", childID2)...)
	// A resume_agent call + its OUTPUT that (worst case) carries an agent_id for
	// childID3. Its call_id is NOT a spawn_agent call, so the spawn-call_id gate
	// must exclude childID3 — this is the live guard against the resume double-count.
	lines = append(lines, functionCallLine(t, "resume_agent", `{"id":"`+childID3+`"}`)) // call_id "call_x"
	lines = append(lines, rolloutLineJSON(t, "response_item", map[string]any{
		"type": "function_call_output", "call_id": "call_x",
		"output": `{"agent_id":"` + childID3 + `","previous_status":"completed"}`,
	}))
	parent := strings.Join(lines, "\n")

	ids := extractSpawnedAgentIDs([]byte(parent), 0)
	require.ElementsMatch(t, []string{childID1, childID2}, ids,
		"only spawn_agent outputs are discovered; a resumed child's output must be excluded")
}

// fromOffset scopes discovery to the current checkpoint range. A subagent
// spawned in a prior range (its spawn output before the offset) must NOT be
// re-attributed — otherwise its files/tokens are re-counted every later turn.
func TestSubagentDiscovery_RespectsFromOffset(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)
	writeChildRollout(t, sessionsDir, childID1,
		sessionMetaLine(t, childID1), applyPatchLine(t, "Add", "child1.go"), tokenCountLine(t, 100, 0, 50))

	lines := []string{sessionMetaLine(t, "parent")}                            // line 1
	lines = append(lines, spawnAgentLines(t, "call_1", "worker", childID1)...) // lines 2-3 (spawn + output)
	lines = append(lines, applyPatchLine(t, "Update", "parent-late.go"))       // line 4 (this range)
	lines = append(lines, tokenCountLine(t, 10, 0, 5))                         // line 5 (this range): keeps usage non-nil
	parent := strings.Join(lines, "\n")

	ag := &CodexAgent{}
	// Offset 3 is past the spawn OUTPUT (line 3): childID1 belongs to a prior range.
	files, err := ag.ExtractAllModifiedFiles([]byte(parent), 3, "")
	require.NoError(t, err)
	require.Equal(t, []string{"parent-late.go"}, files, "subagent spawned before the offset must not be re-attributed")

	usage, err := ag.CalculateTotalTokenUsage([]byte(parent), 3, "")
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Nil(t, usage.SubagentTokens, "subagent tokens before the offset must not be re-attributed")

	// Sanity: with offset 0 the subagent IS attributed.
	files0, err := (&CodexAgent{}).ExtractAllModifiedFiles([]byte(parent), 0, "")
	require.NoError(t, err)
	require.Contains(t, files0, "child1.go")
}

// A spawn whose CALL precedes the offset but whose OUTPUT lands in range must
// still be discovered: extractSpawnedAgentIDs collects spawn call_ids across the
// whole transcript (first pass) precisely so the output line — not the call line
// — decides the checkpoint range. The converse (output at/before the offset) must
// not be discovered.
func TestSubagentDiscovery_SpawnOutputLineDecidesRange(t *testing.T) {
	pair := spawnAgentLines(t, "call_1", "worker", childID1)
	spawnCall, spawnOutput := pair[0], pair[1]
	parent := strings.Join([]string{
		sessionMetaLine(t, "parent"),          // line 1
		spawnCall,                             // line 2 — spawn CALL (before offset)
		applyPatchLine(t, "Update", "mid.go"), // line 3 — filler, separates call from output
		spawnOutput,                           // line 4 — spawn OUTPUT (in range)
	}, "\n")

	// Offset 2 skips the call (line 2) but the output (line 4) is in range; the
	// first pass still has call_1, so the child is discovered.
	require.Equal(t, []string{childID1}, extractSpawnedAgentIDs([]byte(parent), 2),
		"spawn output in range must be discovered even when the call precedes the offset")

	// Offset 4 puts the output at/before the offset → not discovered.
	require.Empty(t, extractSpawnedAgentIDs([]byte(parent), 4),
		"a spawn whose output is at/before the offset must not be discovered")
}

// --- SubagentAwareExtractor: files + tokens ---

func TestExtractAllModifiedFiles_IncludesSubagents(t *testing.T) {
	sessionsDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionsDir)

	writeChildRollout(t, sessionsDir, childID1, applyPatchLine(t, "Add", "child1.go"))
	writeChildRollout(t, sessionsDir, childID2, applyPatchLine(t, "Update", "child2.go"))
	// childID3 is spawned but has no rollout file → must be skipped gracefully.

	lines := []string{sessionMetaLine(t, "parent"), applyPatchLine(t, "Update", "parent.go")}
	lines = append(lines, spawnAgentLines(t, "c1", "worker", childID1)...)
	lines = append(lines, spawnAgentLines(t, "c2", "worker", childID2)...)
	lines = append(lines, spawnAgentLines(t, "c3", "worker", childID3)...)
	parent := strings.Join(lines, "\n")

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

	lines := []string{sessionMetaLine(t, "parent"), tokenCountLine(t, 10, 0, 5)}
	lines = append(lines, spawnAgentLines(t, "c1", "worker", childID1)...)
	lines = append(lines, spawnAgentLines(t, "c2", "worker", childID2)...)
	parent := strings.Join(lines, "\n")

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
