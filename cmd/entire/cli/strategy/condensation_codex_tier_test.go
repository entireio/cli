package strategy

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// codexTierTranscript is a minimal Codex rollout: a turn_context naming the
// serving model (as every real rollout carries before its token_counts)
// followed by a single cumulative token_count of 35000 total input of which
// 20000 cached, and 8000 output. Over an empty baseline that yields
// fresh=15000, cache_read=20000, output=8000 (Codex never reports
// cache-creation). The turn_context keeps the fixture valid for both pricing
// paths: without a ModelUsageCalculator the flat usage falls back to a single
// bucket under the caller's fallback model; with one, the snapshot is
// attributed to gpt-5.5 and applyTierVariant retargets that bucket onto the
// tier-variant id.
const codexTierTranscript = `{"type":"turn_context","payload":{"cwd":"/tmp/repo","model":"gpt-5.5","effort":"high","summary":"auto"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":35000,"cached_input_tokens":20000,"output_tokens":8000,"total_tokens":63000}}}}`

// Expected gpt-5.5 costs for the fixture above, computed from the embedded table:
//
//	standard (5/30, cache_read 0.5): 15000*5 + 20000*0.5 + 8000*30 = 325000  -> $0.325
//	priority (12.5/75, cache_read 1.25): x2.5                              -> $0.8125
//
// The priority table is exactly 2.5x standard, so priority == 2.5*standard is the
// invariant that proves the "-priority" suffix took effect at condensation. This
// mirrors the live regression, where a priority Codex session was persisted at the
// discarded standard $0.154513 instead of the expected priority $0.3862825
// (= 2.5 x $0.154513).
const (
	codexTierStandardCost = 0.325
	codexTierPriorityCost = 0.8125
)

// ctxWithCodexTier returns a context whose settings resolve to the given Codex
// service tier via an explicit worktree root (no chdir, parallel-safe). An empty
// tier writes no settings file, so estimation runs on the embedded defaults.
func ctxWithCodexTier(t *testing.T, tier string) context.Context {
	t.Helper()
	dir := t.TempDir()
	if tier != "" {
		entireDir := filepath.Join(dir, ".entire")
		if err := os.MkdirAll(entireDir, 0o755); err != nil {
			t.Fatalf("mkdir .entire: %v", err)
		}
		body := `{"enabled": true, "pricing": {"codex_service_tier": "` + tier + `"}}`
		if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write settings.json: %v", err)
		}
	}
	return settings.WithWorktreeRoot(context.Background(), dir)
}

// TestCondensationRepricesCodexPriorityTier is the regression for the defect where
// condensation re-derived the committed per-model buckets from the raw model name,
// discarding the turn-end priority-tier pricing. tokenUsageWithCost is the shared
// choke point every condensation/extraction pricing pass funnels through; with the
// "priority" knob it must price the Codex fallback bucket under gpt-5.5-priority.
func TestCondensationRepricesCodexPriorityTier(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypeCodex)
	if err != nil || ag == nil {
		t.Fatalf("GetByAgentType(codex) = %v, %v", ag, err)
	}
	transcript := []byte(codexTierTranscript)

	// Priority: fallback bucket priced under gpt-5.5-priority.
	prioUsage, prioBuckets := tokenUsageWithCost(ctxWithCodexTier(t, "priority"), ag, transcript, 0, "gpt-5.5", agent.AgentTypeCodex)
	assertCost(t, "priority flat", prioUsage.CostUSD, codexTierPriorityCost)
	assertSingleBucket(t, "priority", prioBuckets, "gpt-5.5-priority", codexTierPriorityCost)

	// Standard (no knob): fallback bucket priced under gpt-5.5.
	stdUsage, stdBuckets := tokenUsageWithCost(ctxWithCodexTier(t, ""), ag, transcript, 0, "gpt-5.5", agent.AgentTypeCodex)
	assertCost(t, "standard flat", stdUsage.CostUSD, codexTierStandardCost)
	assertSingleBucket(t, "standard", stdBuckets, "gpt-5.5", codexTierStandardCost)

	// The suffix multiplies cost by exactly 2.5 (priority rates are 2.5x standard).
	if math.Abs(*prioUsage.CostUSD-2.5*(*stdUsage.CostUSD)) > 1e-9 {
		t.Errorf("priority %.10f != 2.5 x standard %.10f", *prioUsage.CostUSD, *stdUsage.CostUSD)
	}

	// A non-Codex agent type is unaffected even with the priority knob present.
	claudeUsage, claudeBuckets := tokenUsageWithCost(ctxWithCodexTier(t, "priority"), ag, transcript, 0, "gpt-5.5", agent.AgentTypeClaudeCode)
	assertCost(t, "non-codex flat", claudeUsage.CostUSD, codexTierStandardCost)
	assertSingleBucket(t, "non-codex", claudeBuckets, "gpt-5.5", codexTierStandardCost)
}

func assertCost(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s cost = nil, want %.10f", label, want)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Fatalf("%s cost = %.10f, want %.10f", label, *got, want)
	}
}

func assertSingleBucket(t *testing.T, label string, buckets []types.ModelUsage, wantModel string, wantCost float64) {
	t.Helper()
	if len(buckets) != 1 {
		t.Fatalf("%s: got %d buckets, want 1", label, len(buckets))
	}
	b := buckets[0]
	if b.Model != wantModel {
		t.Errorf("%s: bucket model = %q, want %q", label, b.Model, wantModel)
	}
	if b.TokenUsage.CostUSD == nil || math.Abs(*b.TokenUsage.CostUSD-wantCost) > 1e-9 {
		t.Errorf("%s: bucket cost = %v, want %.10f", label, b.TokenUsage.CostUSD, wantCost)
	}
	if b.TokenUsage.CostSource != types.CostSourceEstimated {
		t.Errorf("%s: bucket source = %q, want estimated", label, b.TokenUsage.CostSource)
	}
}
