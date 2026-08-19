package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func costPtr(v float64) *float64 { return &v }

// accumulateTokenUsage must carry CostUSD/CostSource forward, copy (not alias)
// the cost pointer when existing is nil, and recurse cost into SubagentTokens.
func TestAccumulateTokenUsage_CarriesCost_ExistingNil(t *testing.T) {
	t.Parallel()
	incoming := &agent.TokenUsage{
		InputTokens: 10,
		CostUSD:     costPtr(0.5),
		CostSource:  types.CostSourceReported,
	}
	got := accumulateTokenUsage(nil, incoming)
	if got.CostUSD == nil || *got.CostUSD != 0.5 {
		t.Fatalf("CostUSD = %v, want 0.5", got.CostUSD)
	}
	if got.CostSource != types.CostSourceReported {
		t.Fatalf("CostSource = %q, want reported", got.CostSource)
	}
	// Must not alias incoming.CostUSD.
	if got.CostUSD == incoming.CostUSD {
		t.Fatal("result aliased incoming.CostUSD")
	}
	*got.CostUSD = 99
	if *incoming.CostUSD != 0.5 {
		t.Fatalf("mutating result mutated incoming: %v", *incoming.CostUSD)
	}
}

func TestAccumulateTokenUsage_SumsCost_MixedSource(t *testing.T) {
	t.Parallel()
	existing := &agent.TokenUsage{InputTokens: 5, CostUSD: costPtr(1.0), CostSource: types.CostSourceReported}
	incoming := &agent.TokenUsage{InputTokens: 3, CostUSD: costPtr(0.25), CostSource: types.CostSourceEstimated}
	got := accumulateTokenUsage(existing, incoming)
	if got.CostUSD == nil || *got.CostUSD != 1.25 {
		t.Fatalf("CostUSD = %v, want 1.25", got.CostUSD)
	}
	if got.CostSource != types.CostSourceMixed {
		t.Fatalf("CostSource = %q, want mixed", got.CostSource)
	}
}

// Two independent aggregates (mirroring state.TokenUsage and
// state.CheckpointTokenUsage in manual_commit_git) that fold the SAME step must
// not share the step's SubagentTokens object; otherwise mutating one aggregate's
// subtree would cross-contaminate the other and the shared step. Subagent tokens
// are "latest snapshot wins" (replace, not sum) because each step already carries
// the cumulative-since-session-start total, so folding the same 100-token step
// twice leaves each aggregate at exactly 100 — not 200 — with each holding its
// own copy of the subtree.
func TestAccumulateTokenUsage_SubagentNoSharedPointerAcrossAggregates(t *testing.T) {
	t.Parallel()
	step := &agent.TokenUsage{
		InputTokens:    10,
		SubagentTokens: &agent.TokenUsage{InputTokens: 100},
	}

	var aggA, aggB *agent.TokenUsage
	// Step 1: seed both aggregates from the same step.
	aggA = accumulateTokenUsage(aggA, step)
	aggB = accumulateTokenUsage(aggB, step)
	// Step 2: fold the same step again into each.
	aggA = accumulateTokenUsage(aggA, step)
	aggB = accumulateTokenUsage(aggB, step)

	if aggA.SubagentTokens.InputTokens != 100 {
		t.Fatalf("aggA subagent input = %d, want 100 (latest snapshot wins, not summed)", aggA.SubagentTokens.InputTokens)
	}
	if aggB.SubagentTokens.InputTokens != 100 {
		t.Fatalf("aggB subagent input = %d, want 100 (latest snapshot wins, not summed)", aggB.SubagentTokens.InputTokens)
	}
	// The incoming step must never be mutated by accumulation.
	if step.SubagentTokens.InputTokens != 100 {
		t.Fatalf("step subagent mutated: %d, want 100", step.SubagentTokens.InputTokens)
	}
	// The two aggregates must not share the same SubagentTokens object.
	if aggA.SubagentTokens == aggB.SubagentTokens {
		t.Fatal("aggregates share the same SubagentTokens pointer")
	}
}

// Subagent usage is "latest snapshot wins" (replace, not sum): each step already
// carries the cumulative-since-session-start subagent total, so the incoming
// step's SubagentTokens (and its cost) supersede whatever was recorded before.
// The snapshot is deep-copied, so the aggregate never aliases the step's subtree.
func TestAccumulateTokenUsage_SubagentReplacedByLatestSnapshot(t *testing.T) {
	t.Parallel()
	existing := &agent.TokenUsage{
		InputTokens:    5,
		SubagentTokens: &agent.TokenUsage{InputTokens: 1, CostUSD: costPtr(0.25), CostSource: types.CostSourceReported},
	}
	incoming := &agent.TokenUsage{
		InputTokens:    3,
		SubagentTokens: &agent.TokenUsage{InputTokens: 2, CostUSD: costPtr(0.5), CostSource: types.CostSourceReported},
	}
	got := accumulateTokenUsage(existing, incoming)
	if got.SubagentTokens == nil {
		t.Fatal("expected SubagentTokens carried")
	}
	// Replace, not sum: the incoming snapshot's subagent tokens and cost win.
	if got.SubagentTokens.InputTokens != 2 {
		t.Fatalf("subagent input = %d, want 2 (latest snapshot wins)", got.SubagentTokens.InputTokens)
	}
	if got.SubagentTokens.CostUSD == nil || *got.SubagentTokens.CostUSD != 0.5 {
		t.Fatalf("subagent CostUSD = %v, want 0.5 (latest snapshot wins)", got.SubagentTokens.CostUSD)
	}
	if got.SubagentTokens.CostSource != types.CostSourceReported {
		t.Fatalf("subagent CostSource = %q, want reported", got.SubagentTokens.CostSource)
	}
	// Deep copy: the aggregate must not alias the incoming step's subtree.
	if got.SubagentTokens == incoming.SubagentTokens {
		t.Fatal("result aliased incoming.SubagentTokens")
	}
}
