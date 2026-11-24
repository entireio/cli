package opencode

import (
	"slices"
	"testing"
)

const testContext = "ENTIRE-CONTEXT"

func transform(sessionID string) map[string]any {
	if sessionID == "" {
		return map[string]any{"transform": map[string]any{}}
	}
	return map[string]any{"transform": map[string]any{"sessionID": sessionID}}
}

func v1Event(eventType string, properties map[string]any) map[string]any {
	return map[string]any{"event": map[string]any{"type": eventType, "properties": properties}}
}

// userMessage is a new user message in ses_a.
func userMessage(messageID string) map[string]any {
	return v1Event("message.updated", map[string]any{"info": map[string]any{"id": messageID, "sessionID": "ses_a", "role": "user"}})
}

func idle(sessionID string) map[string]any {
	return v1Event("session.status", map[string]any{"sessionID": sessionID, "status": map[string]any{"type": "idle"}})
}

func hasContext(system []string) bool { return slices.Contains(system, testContext) }

// Entire's turn-start context reaches every LLM call of the turn: the first
// call may be the title generator, and OpenCode rebuilds the system prompt on
// each step. Other sessions, children, and calls with no session never get
// it, and it is gone after the turn's idle.
func TestPlugin_InjectionLastsForTheTurn(t *testing.T) {
	t.Parallel()
	_, out := runPluginV1(t, []map[string]any{
		userMessage("msg_1"),
		v1Event("session.created", map[string]any{"info": map[string]any{"id": "ses_child", "parentID": "ses_a"}}),
		transform("ses_a"), // title generator
		transform("ses_a"),
		transform("ses_a"),
		transform("ses_child"),
		transform("ses_b"),
		transform(""),
		idle("ses_b"),
		transform("ses_a"),
		idle("ses_a"),
		transform("ses_a"),
	}, testContext)
	prompts := systemPrompts(t, out)
	want := []bool{true, true, true, false, false, false, true, false}
	if len(prompts) != len(want) {
		t.Fatalf("got %d system prompts, want %d: %v", len(prompts), len(want), prompts)
	}
	for i, w := range want {
		if hasContext(prompts[i]) != w {
			t.Errorf("call %d: system = %v, want context %v", i, prompts[i], w)
		}
	}
}

// A turn that ends before any LLM call keeps its context for the next call:
// Entire does not send it again.
func TestPlugin_UnusedInjectionSurvivesIdle(t *testing.T) {
	t.Parallel()
	_, out := runPluginV1(t, []map[string]any{
		userMessage("msg_1"),
		idle("ses_a"),
		transform("ses_a"),
	}, testContext)
	prompts := systemPrompts(t, out)
	if len(prompts) != 1 || !hasContext(prompts[0]) {
		t.Fatalf("system prompts = %v, want the context kept", prompts)
	}
}

// A later turn's context joins unused context rather than replacing it:
// Entire sends each part only once.
func TestPlugin_UnusedInjectionJoinsTheNextTurns(t *testing.T) {
	t.Parallel()
	_, out := runPluginV1(t, []map[string]any{
		userMessage("msg_1"),
		idle("ses_a"),
		userMessage("msg_2"),
		transform("ses_a"),
	}, testContext)
	prompts := systemPrompts(t, out)
	want := []string{"base", testContext + "\n\n" + testContext}
	if len(prompts) != 1 || !slices.Equal(prompts[0], want) {
		t.Fatalf("system prompts = %q, want %q", prompts, want)
	}
}

// OpenCode 2: likewise, a later turn's context joins unused context.
func TestPluginV2_UnusedInjectionJoinsTheNextTurns(t *testing.T) {
	t.Parallel()
	_, out := runPluginV2Output(t, map[string]any{
		"parents": parentsJSON(map[string]string{"ses_a": ""}),
		"steps": []map[string]any{
			v2Prompt("ses_a", "go"),
			v2Event("session.execution.started", map[string]any{"sessionID": "ses_a"}),
			v2Execution("ses_a", "failed"),
			v2Prompt("ses_a", "again"),
			v2Context(),
		},
	}, testContext)
	prompts := systemPrompts(t, out)
	want := []string{testContext + "\n\n" + testContext}
	if len(prompts) != 1 || !slices.Equal(prompts[0], want) {
		t.Fatalf("system prompts = %q, want %q", prompts, want)
	}
}

// v2Context is an LLM call of ses_a.
func v2Context() map[string]any {
	return map[string]any{"context": map[string]any{"sessionID": "ses_a", "model": map[string]any{"id": "m1", "providerID": "p"}, "system": []any{}}}
}

// OpenCode 2: the context hook adds the turn's context to every LLM call of
// the turn and stops after the execution ends.
func TestPluginV2_InjectionLastsForTheTurn(t *testing.T) {
	t.Parallel()
	_, out := runPluginV2Output(t, map[string]any{
		"parents": parentsJSON(map[string]string{"ses_a": ""}),
		"steps": []map[string]any{
			v2Prompt("ses_a", "go"),
			v2Event("session.execution.started", map[string]any{"sessionID": "ses_a"}),
			v2Context(),
			v2Context(),
			v2Execution("ses_a", "succeeded"),
			v2Context(),
		},
	}, testContext)
	prompts := systemPrompts(t, out)
	want := []bool{true, true, false}
	if len(prompts) != len(want) {
		t.Fatalf("got %d system prompts, want %d: %v", len(prompts), len(want), prompts)
	}
	for i, w := range want {
		if hasContext(prompts[i]) != w {
			t.Errorf("call %d: system = %v, want context %v", i, prompts[i], w)
		}
	}
}

// OpenCode 2: an execution that ends before any LLM call keeps the context.
func TestPluginV2_UnusedInjectionSurvivesTurnEnd(t *testing.T) {
	t.Parallel()
	_, out := runPluginV2Output(t, map[string]any{
		"parents": parentsJSON(map[string]string{"ses_a": ""}),
		"steps": []map[string]any{
			v2Prompt("ses_a", "go"),
			v2Event("session.execution.started", map[string]any{"sessionID": "ses_a"}),
			v2Execution("ses_a", "failed"),
			v2Context(),
		},
	}, testContext)
	prompts := systemPrompts(t, out)
	if len(prompts) != 1 || !hasContext(prompts[0]) {
		t.Fatalf("system prompts = %v, want the context kept", prompts)
	}
}
