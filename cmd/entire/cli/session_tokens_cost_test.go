package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// costPtr and testDisplayPricingTable are defined in checkpoint_tokens_cost_test.go (same package).

func TestFormatCostUSD_Matrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		v      *float64
		source string
		want   string
	}{
		{"nil is empty", nil, types.CostSourceEstimated, ""},
		{"known zero", costPtr(0), "", "$0.00"},
		{"two dp estimated is local", costPtr(0.42), types.CostSourceEstimated, "$0.42 (estimated locally)"},
		{"trailing zero padded reported", costPtr(1.5), types.CostSourceReported, "$1.50 (reported)"},
		{"whole dollars mixed is partial local", costPtr(3), types.CostSourceMixed, "$3.00 (estimated locally, partial)"},
		{"sub-cent estimated is local", costPtr(0.004), types.CostSourceEstimated, "<$0.01 (estimated locally)"},
		{"sub-cent no source", costPtr(0.001), "", "<$0.01"},
		{"cent boundary rounds to a cent", costPtr(0.005), types.CostSourceReported, "$0.01 (reported)"},
		{"unknown source has no suffix", costPtr(0.42), "weird", "$0.42"},
		{"empty source has no suffix", costPtr(0.42), "", "$0.42"},
		{"large amount estimated is local", costPtr(1234.5), types.CostSourceEstimated, "$1234.50 (estimated locally)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatCostUSD(tc.v, tc.source); got != tc.want {
				t.Errorf("formatCostUSD(%v, %q) = %q, want %q", tc.v, tc.source, got, tc.want)
			}
		})
	}
}

// buildSessionTokensReport must recompute a LOCAL cost estimate from the
// session's token breakdown (the CLI no longer persists cost) and label it as a
// local estimate. The per-model breakdown carries 420000 priceable tokens under
// test-model ($1/MTok) => $0.42.
func TestBuildSessionTokensReport_RecomputesLocalCostEstimate(t *testing.T) {
	t.Parallel()

	state := makeSessionState("cost-session", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.ModelName = "test-model"
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:  400000,
		OutputTokens: 20000,
		APICallCount: 3,
	}
	state.ModelUsage = map[string]*agent.TokenUsage{
		"test-model": {InputTokens: 400000, OutputTokens: 20000},
	}

	report := buildSessionTokensReport(state, "active", testDisplayPricingTable(t))
	if report.Tokens == nil || report.Tokens.CostUSD == nil || *report.Tokens.CostUSD != 0.42 {
		t.Fatalf("expected recomputed cost 0.42, got %+v", report.Tokens)
	}
	if report.Tokens.CostSource != types.CostSourceEstimated {
		t.Fatalf("cost source = %q, want estimated", report.Tokens.CostSource)
	}

	var buf bytes.Buffer
	writeSessionTokensText(&buf, report)
	out := buf.String()
	if !strings.Contains(out, "Cost:  $0.42 (estimated locally)") {
		t.Fatalf("expected local-estimate cost line, got:\n%s", out)
	}
	if !strings.Contains(out, localCostEstimateNote) {
		t.Fatalf("expected local-estimate note, got:\n%s", out)
	}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"cost_usd":0.42`) {
		t.Fatalf("expected cost_usd in JSON, got: %s", data)
	}
	if !strings.Contains(string(data), `"cost_source":"estimated"`) {
		t.Fatalf("expected cost_source in JSON, got: %s", data)
	}
}

// When there is no pricing table (estimation disabled) or no priceable model, no
// cost is shown — never $0.
func TestBuildSessionTokensReport_OmitsCostWhenUnpriceable(t *testing.T) {
	t.Parallel()

	state := makeSessionState("no-cost-session", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:  1000,
		OutputTokens: 100,
		APICallCount: 3,
	}

	// nil table => estimation disabled => no cost.
	report := buildSessionTokensReport(state, "active", nil)
	if report.Tokens == nil {
		t.Fatal("expected token usage")
	}
	if report.Tokens.CostUSD != nil {
		t.Fatalf("expected nil cost, got %v", report.Tokens.CostUSD)
	}

	var buf bytes.Buffer
	writeSessionTokensText(&buf, report)
	if strings.Contains(buf.String(), "Cost:") {
		t.Fatalf("expected no cost line, got:\n%s", buf.String())
	}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "cost_usd") {
		t.Fatalf("expected cost_usd omitted, got: %s", data)
	}
}

func TestAgentBriefUsageLine_AppendsLocalEstimateWhenKnown(t *testing.T) {
	t.Parallel()

	withCost := agentBriefUsageLine(&sessionTokensUsage{
		Total:      1000,
		APICalls:   3,
		CostUSD:    costPtr(1.5),
		CostSource: types.CostSourceEstimated,
	})
	if !strings.Contains(withCost, "Cost: $1.50 (estimated locally).") {
		t.Fatalf("expected local-estimate cost clause, got: %q", withCost)
	}

	withoutCost := agentBriefUsageLine(&sessionTokensUsage{Total: 1000, APICalls: 3})
	if strings.Contains(withoutCost, "Cost:") {
		t.Fatalf("expected no cost clause, got: %q", withoutCost)
	}
}
