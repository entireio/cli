package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
)

func costPtr(v float64) *float64 { return &v }

// testDisplayPricingTable is a deterministic table for the display-side local
// cost estimate: "test-model" prices every token class at $1/MTok (provider
// "test" has no explicit cache rates, so cache tokens fall back to the input
// rate). A usage of input+output = N tokens therefore estimates to N/1e6 USD.
func testDisplayPricingTable(t *testing.T) *pricing.Table {
	t.Helper()
	table, err := pricing.LoadTable([]pricing.ModelRate{
		{ID: "test-model", Provider: "test", InputPerMTok: 1, OutputPerMTok: 1},
	})
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	return table
}

// addCheckpointTokenUsage must sum CostUSD, merge CostSource, and carry cost
// through the SubagentTokens recursion.
func TestAddCheckpointTokenUsage_CarriesCost(t *testing.T) {
	t.Parallel()
	a := &agent.TokenUsage{
		InputTokens: 10,
		CostUSD:     costPtr(0.50),
		CostSource:  types.CostSourceReported,
		SubagentTokens: &agent.TokenUsage{
			InputTokens: 1,
			CostUSD:     costPtr(0.125),
			CostSource:  types.CostSourceReported,
		},
	}
	b := &agent.TokenUsage{
		InputTokens: 20,
		CostUSD:     costPtr(0.25),
		CostSource:  types.CostSourceEstimated,
		SubagentTokens: &agent.TokenUsage{
			InputTokens: 2,
			CostUSD:     costPtr(0.375),
			CostSource:  types.CostSourceReported,
		},
	}
	got := addCheckpointTokenUsage(a, b)
	if got.CostUSD == nil || *got.CostUSD != 0.75 {
		t.Fatalf("CostUSD = %v, want 0.75", got.CostUSD)
	}
	if got.CostSource != types.CostSourceMixed {
		t.Fatalf("CostSource = %q, want mixed", got.CostSource)
	}
	if got.SubagentTokens == nil {
		t.Fatal("expected SubagentTokens carried")
	}
	if got.SubagentTokens.CostUSD == nil || *got.SubagentTokens.CostUSD != 0.5 {
		t.Fatalf("subagent CostUSD = %v, want 0.5", *got.SubagentTokens.CostUSD)
	}
	if got.SubagentTokens.CostSource != types.CostSourceReported {
		t.Fatalf("subagent CostSource = %q, want reported", got.SubagentTokens.CostSource)
	}
}

func TestAddCheckpointTokenUsage_NilCostStaysNil(t *testing.T) {
	t.Parallel()
	a := &agent.TokenUsage{InputTokens: 1}
	b := &agent.TokenUsage{InputTokens: 2}
	got := addCheckpointTokenUsage(a, b)
	if got.CostUSD != nil {
		t.Fatalf("CostUSD = %v, want nil", got.CostUSD)
	}
	if got.CostSource != "" {
		t.Fatalf("CostSource = %q, want empty", got.CostSource)
	}
}

// buildCheckpointTokensReport must recompute a LOCAL cost estimate from the
// persisted token breakdown (the CLI no longer persists cost) and label it as a
// local estimate. Here the per-model breakdown carries 420000 priceable tokens
// under test-model ($1/MTok) => $0.42.
func TestBuildCheckpointTokensReport_RecomputesLocalCostEstimate(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("abc123abc123")
	report := buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "0/metadata.json"}},
		},
		[]*checkpoint.Metadata{
			{
				SessionID: "cost-session",
				Agent:     "Claude Code",
				Model:     "test-model",
				// Persisted usage carries NO cost (stripped on write); only tokens.
				TokenUsage: &agent.TokenUsage{InputTokens: 400000, OutputTokens: 20000},
				ModelUsage: []types.ModelUsage{
					{Model: "test-model", TokenUsage: agent.TokenUsage{InputTokens: 400000, OutputTokens: 20000}},
				},
			},
		},
		0,
		testDisplayPricingTable(t),
	)
	if report.Tokens == nil || report.Tokens.CostUSD == nil || *report.Tokens.CostUSD != 0.42 {
		t.Fatalf("expected recomputed cost 0.42, got %+v", report.Tokens)
	}
	if report.Tokens.CostSource != types.CostSourceEstimated {
		t.Fatalf("cost source = %q, want estimated", report.Tokens.CostSource)
	}

	var buf bytes.Buffer
	writeCheckpointTokensText(&buf, report)
	out := buf.String()
	if !strings.Contains(out, "Cost:  $0.42 (estimated locally)") {
		t.Fatalf("expected local-estimate cost line, got:\n%s", out)
	}
	if !strings.Contains(out, localCostEstimateNote) {
		t.Fatalf("expected local-estimate note, got:\n%s", out)
	}
}

// With no pricing table (estimation disabled) and no priceable model, no cost is
// shown — never $0.
func TestBuildCheckpointTokensReport_NoCostWhenUnpriceable(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("abc123abc124")
	report := buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "0/metadata.json"}},
		},
		[]*checkpoint.Metadata{
			{
				SessionID:  "cost-session",
				Agent:      "Claude Code",
				Model:      "test-model",
				TokenUsage: &agent.TokenUsage{InputTokens: 400000, OutputTokens: 20000},
			},
		},
		0,
		nil, // estimation disabled
	)
	if report.Tokens == nil {
		t.Fatal("expected token usage")
	}
	if report.Tokens.CostUSD != nil {
		t.Fatalf("expected no cost when unpriceable, got %v", report.Tokens.CostUSD)
	}
	var buf bytes.Buffer
	writeCheckpointTokensText(&buf, report)
	if strings.Contains(buf.String(), "Cost:") {
		t.Fatalf("expected no cost line, got:\n%s", buf.String())
	}
}

// --compare emits a cost delta only when both checkpoints carry a cost.
func TestBuildCheckpointTokensComparison_CostDeltaBothSides(t *testing.T) {
	t.Parallel()
	baseline := checkpointTokensReport{
		CheckpointID: "aaa111bbb222",
		Tokens:       &sessionTokensUsage{Total: 1000, Input: 1000, CostUSD: costPtr(0.50), CostSource: types.CostSourceEstimated},
	}
	target := checkpointTokensReport{
		CheckpointID: "bbb222ccc333",
		Tokens:       &sessionTokensUsage{Total: 800, Input: 800, CostUSD: costPtr(0.30), CostSource: types.CostSourceEstimated},
	}

	cmp := buildCheckpointTokensComparison(target, baseline)
	if cmp.Cost == nil {
		t.Fatal("expected cost delta")
	}
	if cmp.CostCaveat != "" {
		t.Fatalf("expected no cost caveat, got %q", cmp.CostCaveat)
	}
	if cmp.Cost.Direction != checkpointDeltaDirectionDown {
		t.Fatalf("direction = %q, want down", cmp.Cost.Direction)
	}
	if cmp.Cost.Baseline != 0.50 || cmp.Cost.Current != 0.30 {
		t.Fatalf("baseline/current = %v/%v, want 0.50/0.30", cmp.Cost.Baseline, cmp.Cost.Current)
	}
	if cmp.Cost.ChangePercent == nil || *cmp.Cost.ChangePercent > -39.9 || *cmp.Cost.ChangePercent < -40.1 {
		t.Fatalf("change percent = %v, want ~-40", cmp.Cost.ChangePercent)
	}

	var buf bytes.Buffer
	writeCheckpointTokenComparison(&buf, cmp)
	if !strings.Contains(buf.String(), "Cost: down 40% ($0.50 -> $0.30)") {
		t.Fatalf("expected cost delta line, got:\n%s", buf.String())
	}
}

// --compare omits the delta and notes the asymmetry when exactly one side has cost.
func TestBuildCheckpointTokensComparison_CostCaveatWhenOneSideMissing(t *testing.T) {
	t.Parallel()
	baseline := checkpointTokensReport{
		CheckpointID: "aaa111bbb222",
		Tokens:       &sessionTokensUsage{Total: 1000, Input: 1000, CostUSD: costPtr(0.50), CostSource: types.CostSourceEstimated},
	}
	target := checkpointTokensReport{
		CheckpointID: "bbb222ccc333",
		Tokens:       &sessionTokensUsage{Total: 800, Input: 800},
	}

	cmp := buildCheckpointTokensComparison(target, baseline)
	if cmp.Cost != nil {
		t.Fatalf("expected no cost delta, got %+v", cmp.Cost)
	}
	if cmp.CostCaveat != costNotComparableCaveat {
		t.Fatalf("cost caveat = %q, want %q", cmp.CostCaveat, costNotComparableCaveat)
	}

	var buf bytes.Buffer
	writeCheckpointTokenComparison(&buf, cmp)
	out := buf.String()
	if !strings.Contains(out, "Caveat: cost not comparable: missing on one side") {
		t.Fatalf("expected cost caveat line, got:\n%s", out)
	}
	if strings.Contains(out, "Cost: ") {
		t.Fatalf("expected no cost delta line, got:\n%s", out)
	}
}

// When neither side has cost, cost is simply not tracked: no delta, no caveat noise.
func TestBuildCheckpointTokensComparison_NoCostNoiseWhenBothMissing(t *testing.T) {
	t.Parallel()
	baseline := checkpointTokensReport{CheckpointID: "aaa111bbb222", Tokens: &sessionTokensUsage{Total: 1000, Input: 1000}}
	target := checkpointTokensReport{CheckpointID: "bbb222ccc333", Tokens: &sessionTokensUsage{Total: 800, Input: 800}}

	cmp := buildCheckpointTokensComparison(target, baseline)
	if cmp.Cost != nil || cmp.CostCaveat != "" {
		t.Fatalf("expected no cost delta and no caveat, got cost=%+v caveat=%q", cmp.Cost, cmp.CostCaveat)
	}
}
