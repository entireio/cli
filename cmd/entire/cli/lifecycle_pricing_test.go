package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
)

func TestPricingModelForTier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		agentType types.AgentType
		tier      string
		model     string
		want      string
	}{
		{"codex priority appends variant", agent.AgentTypeCodex, "priority", "gpt-5.5", "gpt-5.5-priority"},
		{"codex priority case-insensitive", agent.AgentTypeCodex, "Priority", "gpt-5.5", "gpt-5.5-priority"},
		{"codex empty tier unchanged", agent.AgentTypeCodex, "", "gpt-5.5", "gpt-5.5"},
		{"codex explicit standard unchanged", agent.AgentTypeCodex, "standard", "gpt-5.5", "gpt-5.5"},
		{"non-codex priority ignored", agent.AgentTypeClaudeCode, "priority", "claude-opus-4-8", "claude-opus-4-8"},
		{"empty model unchanged", agent.AgentTypeCodex, "priority", "", ""},
		{"already-priority not doubled", agent.AgentTypeCodex, "priority", "gpt-5.5-priority", "gpt-5.5-priority"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pricingModelForTier(tc.agentType, tc.tier, tc.model); got != tc.want {
				t.Errorf("pricingModelForTier(%q, %q, %q) = %q, want %q", tc.agentType, tc.tier, tc.model, got, tc.want)
			}
		})
	}
}

// TestCodexPriorityTierPricesAtPremium ties the service-tier knob to the actual
// priced outcome through the real embedded table: with "priority" a Codex turn
// on gpt-5.5 prices under gpt-5.5-priority (12.5/75), without it under gpt-5.5
// (5/30).
func TestCodexPriorityTierPricesAtPremium(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	usage := &agent.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	// Priority knob -> gpt-5.5-priority: 1M@$12.5 + 1M@$75 = 87.5.
	prioModel := pricingModelForTier(agent.AgentTypeCodex, "priority", "gpt-5.5")
	if prioModel != "gpt-5.5-priority" {
		t.Fatalf("priority model = %q, want gpt-5.5-priority", prioModel)
	}
	prioPriced, _ := agent.PriceUsage(usage, prioModel, table, false)
	if prioPriced.CostUSD == nil || *prioPriced.CostUSD != 87.5 {
		t.Fatalf("priority cost = %v, want 87.5", prioPriced.CostUSD)
	}

	// No knob -> gpt-5.5: 1M@$5 + 1M@$30 = 35.
	stdModel := pricingModelForTier(agent.AgentTypeCodex, "", "gpt-5.5")
	if stdModel != "gpt-5.5" {
		t.Fatalf("standard model = %q, want gpt-5.5", stdModel)
	}
	stdPriced, _ := agent.PriceUsage(usage, stdModel, table, false)
	if stdPriced.CostUSD == nil || *stdPriced.CostUSD != 35.0 {
		t.Fatalf("standard cost = %v, want 35.0", stdPriced.CostUSD)
	}
}
