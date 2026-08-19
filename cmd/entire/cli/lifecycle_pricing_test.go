package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// tierSettings builds an EntireSettings carrying only a Codex service tier, the
// single input the pricing seam reads.
func tierSettings(tier string) *settings.EntireSettings {
	return &settings.EntireSettings{Pricing: &settings.PricingSettings{CodexServiceTier: tier}}
}

// TestCodexPriorityTierPricesAtPremium ties the service-tier knob to the actual
// priced outcome through the real embedded table and the lifecycle-facing seam
// (settings.EntireSettings.PricingModelForAgent): with "priority" a Codex turn on
// gpt-5.5 prices under gpt-5.5-priority (12.5/75), without it under gpt-5.5
// (5/30). This is the turn-end analogue of the condensation regression.
func TestCodexPriorityTierPricesAtPremium(t *testing.T) {
	t.Parallel()

	table, err := pricing.LoadTable(nil)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	usage := &agent.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	// Priority knob -> gpt-5.5-priority: 1M@$12.5 + 1M@$75 = 87.5.
	prioModel := tierSettings("priority").PricingModelForAgent(agent.AgentTypeCodex, "gpt-5.5")
	if prioModel != "gpt-5.5-priority" {
		t.Fatalf("priority model = %q, want gpt-5.5-priority", prioModel)
	}
	prioPriced, _ := agent.PriceUsage(usage, prioModel, table, false)
	if prioPriced.CostUSD == nil || *prioPriced.CostUSD != 87.5 {
		t.Fatalf("priority cost = %v, want 87.5", prioPriced.CostUSD)
	}

	// No knob -> gpt-5.5: 1M@$5 + 1M@$30 = 35.
	stdModel := tierSettings("").PricingModelForAgent(agent.AgentTypeCodex, "gpt-5.5")
	if stdModel != "gpt-5.5" {
		t.Fatalf("standard model = %q, want gpt-5.5", stdModel)
	}
	stdPriced, _ := agent.PriceUsage(usage, stdModel, table, false)
	if stdPriced.CostUSD == nil || *stdPriced.CostUSD != 35.0 {
		t.Fatalf("standard cost = %v, want 35.0", stdPriced.CostUSD)
	}
}
