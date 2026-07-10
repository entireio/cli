package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

const (
	testAgentCodex  types.AgentType = "Codex"       // mirrors agent.AgentTypeCodex
	testAgentClaude types.AgentType = "Claude Code" // mirrors agent.AgentTypeClaudeCode

	testModelGPT55         = "gpt-5.5"
	testModelGPT55Priority = "gpt-5.5-priority"
)

// TestPricingModelForAgent covers the tier-suffix seam directly on the settings
// method: only a Codex session with the "priority" knob gets the "-priority"
// pricing variant, and never a double suffix.
func TestPricingModelForAgent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		agentType types.AgentType
		tier      string
		model     string
		want      string
	}{
		{"codex priority appends variant", testAgentCodex, "priority", testModelGPT55, testModelGPT55Priority},
		{"codex priority case-insensitive", testAgentCodex, "Priority", testModelGPT55, testModelGPT55Priority},
		{"codex empty tier unchanged", testAgentCodex, "", testModelGPT55, testModelGPT55},
		{"codex explicit standard unchanged", testAgentCodex, "standard", testModelGPT55, testModelGPT55},
		{"non-codex priority ignored", testAgentClaude, "priority", "claude-opus-4-8", "claude-opus-4-8"},
		{"empty model unchanged", testAgentCodex, "priority", "", ""},
		{"already-priority not doubled", testAgentCodex, "priority", testModelGPT55Priority, testModelGPT55Priority},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &EntireSettings{Pricing: &PricingSettings{CodexServiceTier: tc.tier}}
			if got := s.PricingModelForAgent(tc.agentType, tc.model); got != tc.want {
				t.Errorf("PricingModelForAgent(%q, %q) tier=%q = %q, want %q", tc.agentType, tc.model, tc.tier, got, tc.want)
			}
		})
	}
}

// TestPricingModelForAgent_NilSafe confirms the method never panics on a nil
// receiver or unset pricing settings (both read as the default empty tier).
func TestPricingModelForAgent_NilSafe(t *testing.T) {
	t.Parallel()
	var nilSettings *EntireSettings
	if got := nilSettings.PricingModelForAgent(testAgentCodex, testModelGPT55); got != testModelGPT55 {
		t.Errorf("nil settings = %q, want %q (no tier)", got, testModelGPT55)
	}
	if got := (&EntireSettings{}).PricingModelForAgent(testAgentCodex, testModelGPT55); got != testModelGPT55 {
		t.Errorf("empty settings = %q, want %q (no tier)", got, testModelGPT55)
	}
}

// TestPricingModelForAgentCtx exercises the context-based loader: it reads the
// project settings for the tier, short-circuits non-Codex agents without a load,
// and returns the model unchanged when settings cannot be loaded.
func TestPricingModelForAgentCtx(t *testing.T) {
	t.Parallel()

	writeTier := func(t *testing.T, tier string) context.Context {
		t.Helper()
		dir := t.TempDir()
		entireDir := filepath.Join(dir, ".entire")
		if err := os.MkdirAll(entireDir, 0o755); err != nil {
			t.Fatalf("mkdir .entire: %v", err)
		}
		body := `{"enabled": true, "pricing": {"codex_service_tier": "` + tier + `"}}`
		if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write settings.json: %v", err)
		}
		return WithWorktreeRoot(context.Background(), dir)
	}

	// Codex + project priority knob -> suffixed.
	if got := PricingModelForAgent(writeTier(t, "priority"), testAgentCodex, testModelGPT55); got != testModelGPT55Priority {
		t.Errorf("codex priority via ctx = %q, want %q", got, testModelGPT55Priority)
	}
	// Codex + no knob -> unchanged.
	if got := PricingModelForAgent(writeTier(t, ""), testAgentCodex, testModelGPT55); got != testModelGPT55 {
		t.Errorf("codex standard via ctx = %q, want %q", got, testModelGPT55)
	}
	// Non-Codex short-circuits before any load, even with a priority file present.
	if got := PricingModelForAgent(writeTier(t, "priority"), testAgentClaude, "claude-opus-4-8"); got != "claude-opus-4-8" {
		t.Errorf("non-codex via ctx = %q, want claude-opus-4-8", got)
	}
	// Empty model is returned as-is regardless of settings.
	if got := PricingModelForAgent(writeTier(t, "priority"), testAgentCodex, ""); got != "" {
		t.Errorf("empty model via ctx = %q, want empty", got)
	}
}
