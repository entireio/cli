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

// costPtr is defined in checkpoint_tokens_cost_test.go (same package).

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
		{"two dp estimated", costPtr(0.42), types.CostSourceEstimated, "$0.42 (estimated)"},
		{"trailing zero padded", costPtr(1.5), types.CostSourceReported, "$1.50 (reported)"},
		{"whole dollars mixed", costPtr(3), types.CostSourceMixed, "$3.00 (mixed)"},
		{"sub-cent estimated", costPtr(0.004), types.CostSourceEstimated, "<$0.01 (estimated)"},
		{"sub-cent no source", costPtr(0.001), "", "<$0.01"},
		{"cent boundary rounds to a cent", costPtr(0.005), types.CostSourceReported, "$0.01 (reported)"},
		{"unknown source has no suffix", costPtr(0.42), "weird", "$0.42"},
		{"empty source has no suffix", costPtr(0.42), "", "$0.42"},
		{"large amount", costPtr(1234.5), types.CostSourceEstimated, "$1234.50 (estimated)"},
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

func TestBuildSessionTokensReport_CarriesCost(t *testing.T) {
	t.Parallel()

	state := makeSessionState("cost-session", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.ModelName = testModelClaudeOpus
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:  1000,
		OutputTokens: 100,
		APICallCount: 3,
		CostUSD:      costPtr(0.42),
		CostSource:   types.CostSourceEstimated,
	}

	report := buildSessionTokensReport(state, "active")
	if report.Tokens == nil || report.Tokens.CostUSD == nil || *report.Tokens.CostUSD != 0.42 {
		t.Fatalf("expected cost 0.42, got %+v", report.Tokens)
	}
	if report.Tokens.CostSource != types.CostSourceEstimated {
		t.Fatalf("cost source = %q, want estimated", report.Tokens.CostSource)
	}

	var buf bytes.Buffer
	writeSessionTokensText(&buf, report)
	if !strings.Contains(buf.String(), "Cost:  $0.42 (estimated)") {
		t.Fatalf("expected cost line in text, got:\n%s", buf.String())
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

func TestBuildSessionTokensReport_OmitsCostWhenAbsent(t *testing.T) {
	t.Parallel()

	state := makeSessionState("no-cost-session", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.TokenUsage = &agent.TokenUsage{
		InputTokens:  1000,
		OutputTokens: 100,
		APICallCount: 3,
	}

	report := buildSessionTokensReport(state, "active")
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

func TestAgentBriefUsageLine_AppendsCostWhenKnown(t *testing.T) {
	t.Parallel()

	withCost := agentBriefUsageLine(&sessionTokensUsage{
		Total:      1000,
		APICalls:   3,
		CostUSD:    costPtr(1.5),
		CostSource: types.CostSourceReported,
	})
	if !strings.Contains(withCost, "Cost: $1.50 (reported).") {
		t.Fatalf("expected cost clause, got: %q", withCost)
	}

	withoutCost := agentBriefUsageLine(&sessionTokensUsage{Total: 1000, APICalls: 3})
	if strings.Contains(withoutCost, "Cost:") {
		t.Fatalf("expected no cost clause, got: %q", withoutCost)
	}
}
