package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderAdditionalContextHookOutput(t *testing.T) {
	t.Parallel()

	out, err := RenderAdditionalContextHookOutput("UserPromptSubmit", "use entire trail")
	if err != nil {
		t.Fatalf("RenderAdditionalContextHookOutput: %v", err)
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Errorf("payload must be newline-terminated, got %q", string(out))
	}

	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v (%q)", err, string(out))
	}
	if parsed.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want UserPromptSubmit", parsed.HookSpecificOutput.HookEventName)
	}
	if parsed.HookSpecificOutput.AdditionalContext != "use entire trail" {
		t.Errorf("additionalContext = %q", parsed.HookSpecificOutput.AdditionalContext)
	}
}

func TestRenderAdditionalContextHookOutput_EmptyTextRendersNothing(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"", "   ", "\n\t"} {
		out, err := RenderAdditionalContextHookOutput("BeforeAgent", text)
		if err != nil {
			t.Fatalf("RenderAdditionalContextHookOutput(%q): %v", text, err)
		}
		if len(out) != 0 {
			t.Errorf("empty text %q must render no payload, got %q", text, string(out))
		}
	}
}

// injectorOnly implements just enough of Agent + ContextInjector for the
// capability resolver test.
type injectorStub struct{ Agent }

func (injectorStub) InjectionEvent() EventType { return TurnStart }

//nolint:unparam // signature is dictated by the ContextInjector interface
func (injectorStub) RenderContextInjection(ContextInjection) ([]byte, error) {
	return []byte("x"), nil
}

func TestAsContextInjector(t *testing.T) {
	t.Parallel()

	if ci, ok := AsContextInjector(nil); ok || ci != nil {
		t.Errorf("AsContextInjector(nil) = (%v, %v), want (nil, false)", ci, ok)
	}

	ci, ok := AsContextInjector(injectorStub{})
	if !ok || ci == nil {
		t.Fatalf("AsContextInjector(injector) = (%v, %v), want non-nil/true", ci, ok)
	}
	if got := ci.InjectionEvent(); got != TurnStart {
		t.Errorf("InjectionEvent = %v, want TurnStart", got)
	}
}
