package agent

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
)

// fakeSubagentModelUsageAgent is the shape a real subagent-spawning agent has
// (Claude Code, Factory AI Droid): it implements BOTH SubagentAwareExtractor —
// so its flat total carries a cumulative-since-session-start SubagentTokens
// subtree — AND ModelUsageCalculator, whose per-model buckets are per-window
// deltas that see only the main transcript. That combination is what makes the
// remainder bucket carry the subagent tokens (and nothing else).
type fakeSubagentModelUsageAgent struct {
	mockBaseAgent

	flat    *TokenUsage
	buckets []types.ModelUsage
}

func (f *fakeSubagentModelUsageAgent) ExtractAllModifiedFiles([]byte, int, string) ([]string, error) {
	return nil, nil
}

//nolint:unparam // test mock: error result satisfies SubagentAwareExtractor
func (f *fakeSubagentModelUsageAgent) CalculateTotalTokenUsage([]byte, int, string) (*TokenUsage, error) {
	return f.flat, nil
}

//nolint:unparam // test mock: error result satisfies ModelUsageCalculator
func (f *fakeSubagentModelUsageAgent) CalculateModelUsage([]byte, int) ([]types.ModelUsage, error) {
	return f.buckets, nil
}

// anthropicTestTable returns a table with one anthropic-provider model at
// $1/MTok input and output and NO explicit cache rates, so Estimate applies the
// Anthropic multipliers: cache read 0.1x, 5-minute cache write 1.25x, and
// 1-hour cache write 2x. That makes a 1h-vs-5m mispricing visible as an exact
// dollar difference.
func anthropicTestTable(t *testing.T) *pricing.Table {
	t.Helper()
	table, err := pricing.LoadTable([]pricing.ModelRate{
		{ID: "anth-test", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
	})
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	return table
}

// TestCalculateUsageWithCost_RemainderRescopedToAccountedSubagentSnapshot is the
// regression test for the unbounded additive inflation of subagent cost across a
// multi-turn session.
//
// flat.SubagentTokens is a cumulative-since-session-start snapshot (the
// SubagentAwareExtractor contract), while flat's own scalars and every per-model
// bucket are per-window deltas. remainderBucket used to flatten the raw
// cumulative subtree into its shortfall, so every turn re-attributed the WHOLE
// subagent total. Callers sum buckets and cost across turns, so a session with a
// subagent that stopped working after turn 1 still grew its cost linearly with
// the turn count. Threading the previously-accounted snapshot in makes the
// remainder a true increment: full on turn 1, absent while the snapshot is
// unchanged, and exactly the growth when it grows.
func TestCalculateUsageWithCost_RemainderRescopedToAccountedSubagentSnapshot(t *testing.T) {
	t.Parallel()

	// Per turn the main agent spends 1M input tokens ($1.00 at test-a's rate).
	mainPerTurn := func() []types.ModelUsage {
		return []types.ModelUsage{{Model: "test-a", TokenUsage: TokenUsage{InputTokens: 1_000_000, APICallCount: 1}}}
	}

	turns := []struct {
		name string
		// subagentCumulative is what CalculateTotalTokenUsage reports as the
		// cumulative subagent total at this turn.
		subagentCumulative int
		wantRemainder      int
		wantTurnCost       float64
	}{
		// Turn 1 discovers the subagent: the whole 5M snapshot is new.
		{"turn1-discovers", 5_000_000, 5_000_000, 6.0},
		// Turns 2 and 3 see the identical snapshot (the subagent did no more
		// work, but its transcript is still re-read from line 0). Nothing new to
		// attribute, so there must be NO remainder bucket at all.
		{"turn2-flat", 5_000_000, 0, 1.0},
		{"turn3-flat", 5_000_000, 0, 1.0},
		// Turn 4 the subagent runs again: only the 2M growth is new.
		{"turn4-grows", 7_000_000, 2_000_000, 3.0},
	}

	// accounted tracks what the caller has already attributed, exactly as
	// state.TokenUsage.SubagentTokens does on the live turn-end path (replace,
	// never add).
	var accounted *TokenUsage
	var totalCost float64
	var totalAttributedTokens int

	for _, turn := range turns {
		ag := &fakeSubagentModelUsageAgent{
			flat: &TokenUsage{
				InputTokens:    1_000_000,
				APICallCount:   1,
				SubagentTokens: &TokenUsage{InputTokens: turn.subagentCumulative, APICallCount: 5},
			},
			buckets: mainPerTurn(),
		}

		flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "subagents", accounted, testTable(t), "test-a", false)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", turn.name, err)
		}

		var gotRemainder int
		switch turn.wantRemainder {
		case 0:
			if len(buckets) != 1 {
				t.Fatalf("%s: buckets = %d (%+v), want 1 (no remainder bucket for an unchanged snapshot)",
					turn.name, len(buckets), buckets)
			}
		default:
			if len(buckets) != 2 {
				t.Fatalf("%s: buckets = %d (%+v), want 2 (main + remainder)", turn.name, len(buckets), buckets)
			}
			gotRemainder = buckets[1].TokenUsage.InputTokens
		}
		if gotRemainder != turn.wantRemainder {
			t.Errorf("%s: remainder InputTokens = %d, want %d", turn.name, gotRemainder, turn.wantRemainder)
		}
		if flat.CostUSD == nil || *flat.CostUSD != turn.wantTurnCost {
			t.Fatalf("%s: turn cost = %v, want %v", turn.name, flat.CostUSD, turn.wantTurnCost)
		}

		// The flat usage's own subtree must still be the CUMULATIVE snapshot:
		// callers replace it wholesale, and rescoping must not have mutated it.
		if flat.SubagentTokens == nil || flat.SubagentTokens.InputTokens != turn.subagentCumulative {
			t.Fatalf("%s: flat.SubagentTokens = %+v, want the cumulative snapshot %d",
				turn.name, flat.SubagentTokens, turn.subagentCumulative)
		}

		totalCost += *flat.CostUSD
		for i := range buckets {
			totalAttributedTokens += buckets[i].TokenUsage.InputTokens
		}
		accounted = flat.SubagentTokens
	}

	// Summed across the four turns: 4M main + 7M subagent = 11M input = $11.00.
	// The pre-fix behavior re-attributed the snapshot every turn: 4M main plus
	// 5M+5M+5M+7M = 22M subagent = $26.00 over 26M tokens.
	if totalCost != 11.0 {
		t.Errorf("accumulated cost = %v, want 11.0 (4 turns of main + one 7M subagent total); "+
			"26.0 is the pre-fix value that re-attributes the snapshot every turn", totalCost)
	}
	if totalAttributedTokens != 11_000_000 {
		t.Errorf("accumulated attributed tokens = %d, want 11_000_000 (26_000_000 is the pre-fix value)",
			totalAttributedTokens)
	}
}

// TestCalculateUsageWithCost_RemainderKeepsCacheCreation1hAt2x proves
// CacheCreation1hTokens survives the remainder path and is billed at the 2x
// 1-hour cache-write rate rather than the 1.25x 5-minute rate. remainderBucket
// used to build its shortfall without the field, so any cache-creation tokens
// attributed through the remainder (i.e. every subagent's) silently priced 1.25x
// — a 37.5% undercount on that portion.
func TestCalculateUsageWithCost_RemainderKeepsCacheCreation1hAt2x(t *testing.T) {
	t.Parallel()

	// All 1M cache-creation tokens live in the subagent subtree and all were
	// written with a 1-hour TTL, so the whole cost comes through the remainder.
	ag := &fakeSubagentAwareAgent{usage: &TokenUsage{
		SubagentTokens: &TokenUsage{
			CacheCreationTokens:   1_000_000,
			CacheCreation1hTokens: 1_000_000,
			APICallCount:          3,
		},
	}}

	flat, buckets, err := CalculateUsageWithCost(ag, nil, 0, "subagents", nil, anthropicTestTable(t), "anth-test", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d (%+v), want 2 (zero-token fallback + remainder)", len(buckets), buckets)
	}
	rem := buckets[1].TokenUsage
	if rem.CacheCreation1hTokens != 1_000_000 {
		t.Fatalf("remainder CacheCreation1hTokens = %d, want 1_000_000 (dropped by the pre-fix shortfall)",
			rem.CacheCreation1hTokens)
	}
	// 1M @ 2x $1/MTok = $2.00. The pre-fix value is $1.25 (all writes priced at
	// the 5-minute 1.25x rate because the 1h subset was zeroed).
	if flat.CostUSD == nil || *flat.CostUSD != 2.0 {
		t.Fatalf("cost = %v, want 2.0 at the 2x 1-hour cache-write rate (1.25 is the pre-fix 5-minute rate)",
			flat.CostUSD)
	}
}

// TestRemainderBucket_CacheCreation1hSubtractedFromBucketSum proves the 1h
// subset is subtracted from the per-model bucket sum too, not just added to the
// flat side. Summing it on one side only would attribute a bucket's own 1h
// tokens a second time through the remainder.
func TestRemainderBucket_CacheCreation1hSubtractedFromBucketSum(t *testing.T) {
	t.Parallel()

	flat := &TokenUsage{
		CacheCreationTokens:   1_000_000,
		CacheCreation1hTokens: 1_000_000,
		SubagentTokens: &TokenUsage{
			CacheCreationTokens:   3_000_000,
			CacheCreation1hTokens: 2_000_000,
			APICallCount:          1,
		},
	}
	buckets := []types.ModelUsage{{Model: "anth-test", TokenUsage: TokenUsage{
		CacheCreationTokens:   1_000_000,
		CacheCreation1hTokens: 1_000_000,
	}}}

	rem, ok := remainderBucket(flat, nil, buckets, "anth-test")
	if !ok {
		t.Fatal("remainderBucket returned no bucket for a real cache-creation shortfall")
	}
	if rem.TokenUsage.CacheCreationTokens != 3_000_000 {
		t.Errorf("remainder CacheCreationTokens = %d, want 3_000_000", rem.TokenUsage.CacheCreationTokens)
	}
	if rem.TokenUsage.CacheCreation1hTokens != 2_000_000 {
		t.Errorf("remainder CacheCreation1hTokens = %d, want 2_000_000 (4M flattened less the bucket's own 1M)",
			rem.TokenUsage.CacheCreation1hTokens)
	}
}

// TestEstimateCost_FlatFallbackKeepsCacheCreation1hAt2x covers
// flattenTokenUsage's OTHER consumer: EstimateCost's no-buckets fallback, which
// flattens the subagent subtree into a single bucket. Dropping the 1h subset
// there made `entire session tokens` / `entire checkpoint tokens` render a cost
// estimate that undercharged every 1h cache write.
func TestEstimateCost_FlatFallbackKeepsCacheCreation1hAt2x(t *testing.T) {
	t.Parallel()

	usage := &types.TokenUsage{
		CacheCreationTokens:   400_000,
		CacheCreation1hTokens: 400_000,
		SubagentTokens: &types.TokenUsage{
			CacheCreationTokens:   600_000,
			CacheCreation1hTokens: 600_000,
		},
	}

	cost, source := EstimateCost(usage, nil, "anth-test", anthropicTestTable(t))
	if cost == nil {
		t.Fatal("EstimateCost returned no cost")
	}
	// 1M total cache creation, all 1h: 1M @ 2x $1/MTok = $2.00 ($1.25 pre-fix).
	if *cost != 2.0 {
		t.Errorf("cost = %v, want 2.0 at the 2x 1-hour rate (1.25 is the pre-fix 5-minute rate)", *cost)
	}
	if source != types.CostSourceEstimated {
		t.Errorf("source = %q, want estimated", source)
	}
}

// TestSubtractTokenUsage_RescopesCacheCreation1h pins the field on the rescoping
// helper the remainder fix depends on. SubtractTokenUsage dropped
// CacheCreation1hTokens, so every rescoped delta (this fix's subagent increment,
// and CheckpointTokenUsage.SubagentTokens on the live path) claimed all of its
// cache writes were 5-minute TTL.
func TestSubtractTokenUsage_RescopesCacheCreation1h(t *testing.T) {
	t.Parallel()

	cumulative := &types.TokenUsage{CacheCreationTokens: 900_000, CacheCreation1hTokens: 700_000}
	accounted := &types.TokenUsage{CacheCreationTokens: 400_000, CacheCreation1hTokens: 300_000}

	delta := types.SubtractTokenUsage(cumulative, accounted)
	if delta == nil {
		t.Fatal("SubtractTokenUsage returned nil")
	}
	if delta.CacheCreationTokens != 500_000 {
		t.Errorf("delta CacheCreationTokens = %d, want 500_000", delta.CacheCreationTokens)
	}
	if delta.CacheCreation1hTokens != 400_000 {
		t.Errorf("delta CacheCreation1hTokens = %d, want 400_000 (0 pre-fix)", delta.CacheCreation1hTokens)
	}
}
