package types

import (
	"errors"
	"testing"
)

func TestEventTaxonomy(t *testing.T) {
	t.Parallel()
	// All event variants must implement Event (sealed via private isEvent()).
	var events = []Event{
		Started{},
		AssistantText{Text: "hello"},
		ToolCall{Name: "read", Args: "file.go"},
		Tokens{In: 10, Out: 20},
		Finished{Success: true},
		RunError{Err: errors.New("boom")},
	}
	if len(events) != 6 {
		t.Fatalf("expected 6 event variants, got %d", len(events))
	}
}

func TestEventSwitch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"Started", Started{}, "started"},
		{"AssistantText", AssistantText{Text: "narrative"}, "text"},
		{"ToolCall", ToolCall{Name: "read"}, "tool"},
		{"Tokens", Tokens{In: 1, Out: 2}, "tokens"},
		{"Finished", Finished{Success: true}, "finished"},
		{"RunError", RunError{Err: errors.New("x")}, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			switch tc.ev.(type) {
			case Started:
				got = "started"
			case AssistantText:
				got = "text"
			case ToolCall:
				got = "tool"
			case Tokens:
				got = "tokens"
			case Finished:
				got = "finished"
			case RunError:
				got = "error"
			default:
				t.Fatalf("unexpected event type %T", tc.ev)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunConfigZeroValueIsValid(t *testing.T) {
	t.Parallel()
	// Zero value should be usable — no required-field constructors.
	var c RunConfig
	if len(c.Skills) != 0 {
		t.Errorf("zero RunConfig.Skills should be nil, got %v", c.Skills)
	}
	if c.PromptOverride != "" || c.AlwaysPrompt != "" || c.PerRunPrompt != "" || c.ScopeBaseRef != "" || c.StartingSHA != "" {
		t.Errorf("zero RunConfig fields should be empty strings, got %+v", c)
	}
}

// Compile-time interface satisfaction checks are intentionally deferred to
// CU3's per-agent reviewer tests, which import context.Context correctly.
// Each agent package will include: var _ AgentReviewer = (*ConcreteReviewer)(nil)
