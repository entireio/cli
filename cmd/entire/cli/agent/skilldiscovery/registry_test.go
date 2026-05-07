package skilldiscovery_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
)

func TestCuratedBuiltinsFor_KnownAgents(t *testing.T) {
	t.Parallel()
	claude := skilldiscovery.CuratedBuiltinsFor("claude-code")
	if len(claude) != 3 {
		t.Fatalf("claude-code built-ins: got %d entries, want 3", len(claude))
	}
	codex := skilldiscovery.CuratedBuiltinsFor("codex")
	if len(codex) != 1 || codex[0].Name != "/review" {
		t.Errorf("codex built-ins: got %+v, want 1x /review", codex)
	}
	gemini := skilldiscovery.CuratedBuiltinsFor("gemini-cli")
	if len(gemini) != 0 {
		t.Errorf("gemini-cli built-ins: got %d, want 0", len(gemini))
	}
}

func TestCuratedBuiltinsFor_UnknownAgentReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := skilldiscovery.CuratedBuiltinsFor("nonexistent"); len(got) != 0 {
		t.Errorf("unknown agent: got %d entries, want 0", len(got))
	}
}

func TestActiveInstallHintsFor_SuppressesWhenProvidesAnyDiscovered(t *testing.T) {
	t.Parallel()
	discovered := map[string]struct{}{"/pr-review-toolkit:review-pr": {}}
	hints := skilldiscovery.ActiveInstallHintsFor("claude-code", discovered)
	for _, h := range hints {
		for _, name := range h.ProvidesAny {
			if name == "/pr-review-toolkit:review-pr" {
				t.Errorf("pr-review-toolkit hint should have been suppressed, got %+v", h)
			}
		}
	}
}

func TestActiveInstallHintsFor_ShowsAllWhenNothingDiscovered(t *testing.T) {
	t.Parallel()
	hints := skilldiscovery.ActiveInstallHintsFor("claude-code", nil)
	if len(hints) == 0 {
		t.Fatal("expected at least one hint when discovery set is empty")
	}
}

func TestActiveInstallHintsFor_GeminiAlwaysShownRegardlessOfDiscovery(t *testing.T) {
	t.Parallel()
	hints := skilldiscovery.ActiveInstallHintsFor("gemini-cli", map[string]struct{}{"/anything": {}})
	if len(hints) == 0 {
		t.Error("gemini hint with nil ProvidesAny should always show")
	}
}

func TestIsEligible_IncludesAgentWithOnlyInstallHint(t *testing.T) {
	t.Parallel()
	if !skilldiscovery.IsEligible("gemini-cli") {
		t.Error("gemini-cli should be eligible via install hint alone")
	}
	if !skilldiscovery.IsEligible("claude-code") {
		t.Error("claude-code should be eligible via built-ins")
	}
	if skilldiscovery.IsEligible("nonexistent") {
		t.Error("unknown agent should not be eligible")
	}
}
