package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/plugins"
)

func TestLifecycleHookName_Mapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   agent.EventType
		want string
		ok   bool
	}{
		{agent.SessionStart, plugins.HookSessionStart, true},
		{agent.TurnStart, plugins.HookTurnStart, true},
		{agent.TurnEnd, plugins.HookTurnEnd, true},
		{agent.Compaction, plugins.HookCompaction, true},
		{agent.SessionEnd, plugins.HookSessionEnd, true},
		{agent.SubagentEnd, plugins.HookSubagentEnd, true},
		{agent.ModelUpdate, plugins.HookModelUpdate, true},
		{agent.SubagentStart, "", false},
		{agent.ToolUse, "", false},
	}
	for _, c := range cases {
		got, ok := lifecycleHookName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("lifecycleHookName(%v) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLifecycleHookPayload_OmitsEmptyAndSensitive(t *testing.T) {
	t.Parallel()
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Skipf("claude-code agent not registered: %v", err)
	}
	event := &agent.Event{
		Type:          agent.TurnEnd,
		SessionID:     "sess-1",
		Model:         "claude-sonnet",
		Prompt:        "secret prompt text",
		ModifiedFiles: []string{"a.go", "b.go"},
	}
	payload := lifecycleHookPayload(ag, event)

	if payload["session_id"] != "sess-1" {
		t.Errorf("session_id = %v", payload["session_id"])
	}
	if payload["model"] != "claude-sonnet" {
		t.Errorf("model = %v", payload["model"])
	}
	if _, ok := payload["prompt"]; ok {
		t.Error("payload must not expose prompt text")
	}
	if _, ok := payload["session_ref"]; ok {
		t.Error("empty session_ref should be omitted")
	}
	files, ok := payload["modified_files"].([]string)
	if !ok || len(files) != 2 {
		t.Errorf("modified_files = %v", payload["modified_files"])
	}
}
