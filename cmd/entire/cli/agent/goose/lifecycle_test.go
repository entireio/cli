package goose

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestHookNames(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	names := a.HookNames()
	if len(names) != 4 {
		t.Fatalf("HookNames() returned %d names, want 4: %v", len(names), names)
	}

	// Every advertised verb must have a Goose event behind it, or InstallHooks
	// would write a config that never fires the hook.
	for _, name := range names {
		if _, ok := gooseHookEvents[name]; !ok {
			t.Errorf("hook verb %q has no Goose event mapping", name)
		}
	}
}

func TestGooseHookEvents_CoverRequiredLifecycle(t *testing.T) {
	t.Parallel()

	// These four Goose events are what the integration is built on; all were
	// confirmed present as literals in the v1.46.0 binary (see AGENT.md).
	want := map[string]string{
		HookNameSessionStart: "SessionStart",
		HookNameTurnStart:    "UserPromptSubmit",
		HookNameTurnEnd:      "Stop",
		HookNameSessionEnd:   "SessionEnd",
	}
	for verb, event := range want {
		if got := gooseHookEvents[verb]; got != event {
			t.Errorf("verb %q maps to %q, want %q", verb, got, event)
		}
	}
}

func TestParseHookEvent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		verb      string
		stdin     string
		wantType  agent.EventType
		wantRef   bool // does this event carry a SessionRef?
		wantPromp string
	}{
		{HookNameSessionStart, `{"session_id":"20260819_1"}`, agent.SessionStart, false, ""},
		{HookNameTurnStart, `{"session_id":"20260819_1","prompt":"hi"}`, agent.TurnStart, true, "hi"},
		{HookNameTurnEnd, `{"session_id":"20260819_1"}`, agent.TurnEnd, true, ""},
		{HookNameSessionEnd, `{"session_id":"20260819_1"}`, agent.SessionEnd, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			t.Parallel()

			a := &GooseAgent{}
			event, err := a.ParseHookEvent(context.Background(), tc.verb, strings.NewReader(tc.stdin))
			if err != nil {
				t.Fatalf("ParseHookEvent(%s): %v", tc.verb, err)
			}
			if event == nil {
				t.Fatalf("ParseHookEvent(%s) returned nil event", tc.verb)
			}
			if event.Type != tc.wantType {
				t.Errorf("Type = %v, want %v", event.Type, tc.wantType)
			}
			if event.SessionID != "20260819_1" {
				t.Errorf("SessionID = %q, want 20260819_1", event.SessionID)
			}
			if tc.wantRef && event.SessionRef == "" {
				t.Error("expected a SessionRef for a transcript-bearing event")
			}
			if !tc.wantRef && event.SessionRef != "" {
				t.Errorf("unexpected SessionRef %q", event.SessionRef)
			}
			if event.Prompt != tc.wantPromp {
				t.Errorf("Prompt = %q, want %q", event.Prompt, tc.wantPromp)
			}
		})
	}
}

// The payload's own event-name field is deliberately not consulted: the verb
// already identifies the event, so a renamed or absent field must not break
// parsing.
func TestParseHookEvent_IgnoresPayloadEventField(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	event, err := a.ParseHookEvent(context.Background(), HookNameTurnEnd,
		strings.NewReader(`{"session_id":"s1","event":"SomethingElse"}`))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("Type = %v, want TurnEnd regardless of the payload's event field", event.Type)
	}
}

func TestParseHookEvent_UnknownVerbIsNoOp(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	event, err := a.ParseHookEvent(context.Background(), "not-a-hook", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("unknown hook should not error, got %v", err)
	}
	if event != nil {
		t.Errorf("unknown hook should yield a nil event, got %+v", event)
	}
}

func TestParseHookEvent_EmptyAndMalformedInput(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		if _, err := a.ParseHookEvent(context.Background(), HookNameSessionStart,
			strings.NewReader("")); err == nil {
			t.Error("expected an error for empty hook input")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		if _, err := a.ParseHookEvent(context.Background(), HookNameSessionStart,
			strings.NewReader("{not json")); err == nil {
			t.Error("expected an error for malformed hook input")
		}
	})
}

// A session ID that escapes its directory must be rejected before it reaches a
// path join. The transcript path is built from it directly.
func TestParseHookEvent_RejectsTraversalSessionID(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	_, err := a.ParseHookEvent(context.Background(), HookNameTurnEnd,
		strings.NewReader(`{"session_id":"../../etc/passwd"}`))
	if err == nil {
		t.Fatal("expected a path-traversal session ID to be rejected")
	}
}

func TestPrepareTranscript_RejectsNonJSONPath(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	err := a.PrepareTranscript(context.Background(), "/tmp/session.txt")
	if err == nil {
		t.Fatal("expected a non-.json transcript path to be rejected")
	}
	if !strings.Contains(err.Error(), "expected .json") {
		t.Errorf("error = %v, want it to mention the expected extension", err)
	}
}
