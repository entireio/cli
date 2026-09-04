package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// The renderer prints these verbatim on both commands, so a reason that says
// "checkpoint" is a false statement on `session tokens`. Only the TTL reason is
// legitimately checkpoint-specific: live state always knows the split.
//
// Honest limitation: this is a per-member list of the known unpriced*
// constants. Despite the plural, unqualified test name, it is not exhaustive —
// a fifth unpriced* constant added later would not be covered here
// automatically.
func TestUnpricedReasons_AreScopeNeutral(t *testing.T) {
	t.Parallel()

	// Any noun that presumes one command's scope is a false statement on the
	// other. "checkpoint" was the original slip; "sessions" (plural) is the one
	// the obvious reword introduces, since session tokens has exactly one.
	cases := []struct {
		name   string
		reason string
	}{
		{"no model", unpricedNoModel},
		{"mixed models", unpricedMixedModels},
		{"subagent with no ratios", unpricedSubagentNoRatios},
		{"no cost", unpricedNoCost},
	}
	for _, tc := range cases {
		for _, presumed := range []string{"checkpoint", "sessions"} {
			if strings.Contains(tc.reason, presumed) {
				t.Errorf("%s reason is printed by both commands and must not say %q: %q", tc.name, presumed, tc.reason)
			}
		}
	}

	if !strings.Contains(unpricedUnknownTTL, "checkpoint") {
		t.Error("the TTL reason is checkpoint-only by construction (live state always knows the split) and should keep saying so")
	}
}

// TestWriteTokenClasses_UnpricedReasonIsScopeNeutral covers two things the
// constant-only test above cannot: writeTokenClasses' empty-UnpricedReason
// fallback to unpricedNoModel, and that the scope-word scan below covers the
// whole rendered block rather than just the omitted-cost line — asserting the
// known reasons come through the renderer unchanged is otherwise plumbing
// already covered by the constant comparisons elsewhere.
//
// Honest limitation: this is still a per-member list of the known unpriced*
// constants plus the fallback case. A fifth unpriced* constant added later
// would not be covered here automatically.
func TestWriteTokenClasses_UnpricedReasonIsScopeNeutral(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		unpricedReason string // set on the breakdown; "" exercises the fallback
		wantReason     string // what the rendered line must contain
		checkpointOK   bool   // the TTL case is allowed (expected) to say "checkpoint"
	}{
		{"no model", unpricedNoModel, unpricedNoModel, false},
		{"mixed models", unpricedMixedModels, unpricedMixedModels, false},
		{"subagent with no ratios", unpricedSubagentNoRatios, unpricedSubagentNoRatios, false},
		{"no cost", unpricedNoCost, unpricedNoCost, false},
		// Priced==false with an empty UnpricedReason is currently unreachable in
		// production: tokenClassShares sets a reason on every unpriced branch,
		// and the checkpoint path only ever overwrites a non-empty one. This
		// subtest is defensive coverage of writeTokenClasses' fallback, not a
		// reachable case today — Task 4 adds the second construction site where
		// an empty reason could first appear for real. Keep this subtest even
		// though nothing currently produces its input.
		{"empty reason falls back to no-model", "", unpricedNoModel, false},
		{"unknown TTL", unpricedUnknownTTL, unpricedUnknownTTL, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			classes := &tokenClassBreakdown{
				Input:          tokenClassShare{Tokens: 100, VolumePercent: 100},
				Total:          100,
				Priced:         false,
				UnpricedReason: tc.unpricedReason,
			}

			var buf bytes.Buffer
			writeTokenClasses(&buf, classes)
			out := strings.ToLower(buf.String())

			wantLine := "cost share omitted: " + strings.ToLower(tc.wantReason)
			if !strings.Contains(out, wantLine) {
				t.Errorf("rendered output missing %q, got:\n%s", wantLine, buf.String())
			}

			if tc.checkpointOK {
				return
			}

			for _, presumed := range []string{"checkpoint", "sessions"} {
				if strings.Contains(out, presumed) {
					t.Errorf("rendered block must not say %q, got:\n%s", presumed, buf.String())
				}
			}
		})
	}
}

// A subagent on another provider must unprice a live session exactly as it
// unprices a checkpoint: subagent tokens are folded into the classes, so
// pricing them at the parent's ratio would be a wrong number under Priced:true.
func TestSessionTokenWeights_SubagentOnAnotherProviderIsUnpriced(t *testing.T) {
	t.Parallel()

	usage := &agent.TokenUsage{
		InputTokens: 1000, OutputTokens: 100,
		SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 50, Model: "gpt-5.3-codex"},
	}

	weights, reason := tokenWeightsForSession("claude-sonnet-4.6", usage)
	if weights.Family != "" {
		t.Errorf("family = %q, want empty (unpriced)", weights.Family)
	}
	if reason != unpricedMixedModels {
		t.Errorf("reason = %q, want %q", reason, unpricedMixedModels)
	}
}

func TestSessionTokenWeights_SubagentInSameFamilyStaysPriced(t *testing.T) {
	t.Parallel()

	usage := &agent.TokenUsage{
		InputTokens: 1000, OutputTokens: 100,
		SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 50, Model: "claude-haiku-4-5"},
	}

	weights, reason := tokenWeightsForSession("claude-sonnet-4.6", usage)
	if weights.Family == "" {
		t.Errorf("same family must stay priced; reason was %q", reason)
	}
	if reason != "" {
		t.Errorf("a priced result carries no reason, got %q", reason)
	}
}

// An unrecognised model has genuinely no ratio row: that is the generic reason,
// not the mixed-models one.
func TestSessionTokenWeights_UnknownModelTakesTheGenericReason(t *testing.T) {
	t.Parallel()

	weights, reason := tokenWeightsForSession("some-unknown-model", &agent.TokenUsage{InputTokens: 100})
	if weights.Family != "" {
		t.Errorf("family = %q, want empty", weights.Family)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty so the generic one is used", reason)
	}
}

// A subagent whose model we do not recognise is a different fact from two
// recognised models with differing ratios, and neither existing reason is true
// of it: unpricedMixedModels claims differing ratios when there are none to
// differ from, and unpricedNoModel claims nothing here has verified ratios when
// the parent model does. Hence its own reason. The subagent guard used to
// collapse both cases into one bool and print the mixed-models line for each.
func TestSessionTokenWeights_SubagentWithNoRatiosIsNotAMixedModelsCase(t *testing.T) {
	t.Parallel()

	usage := &agent.TokenUsage{
		InputTokens: 1000, OutputTokens: 100,
		SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 50, Model: "some-unknown-model"},
	}

	weights, reason := tokenWeightsForSession("claude-sonnet-4.6", usage)
	if weights.Family != "" {
		t.Errorf("family = %q, want empty (unpriced)", weights.Family)
	}
	if reason == unpricedMixedModels {
		t.Error("an unrecognised subagent model has no ratios to differ from; that is not the mixed-models case")
	}
	if reason != unpricedSubagentNoRatios {
		t.Errorf("reason = %q, want %q", reason, unpricedSubagentNoRatios)
	}
}

// liveSessionFor builds a live session state with the given model and usage.
// Live state is the current binary's own struct, so unlike a committed
// checkpoint it has no version to be legacy at.
func liveSessionFor(t *testing.T, model string, usage *agent.TokenUsage) *strategy.SessionState {
	t.Helper()
	return &strategy.SessionState{
		SessionID:  "s1",
		AgentType:  "Claude Code",
		ModelName:  model,
		Phase:      session.PhaseIdle,
		TokenUsage: usage,
	}
}

// The point of the feature: a live session on a model we have ratios for gets
// the same volume-and-cost table `checkpoint tokens` shows.
func TestSessionTokensReport_Classes_PricedWhenModelKnown(t *testing.T) {
	t.Parallel()

	report := buildSessionTokensReport(liveSessionFor(t, "claude-sonnet-4.6",
		&agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, CacheReadTokens: 6000, OutputTokens: 1000}), "idle")

	if report.Classes == nil {
		t.Fatal("a live session with usage must carry a class breakdown")
	}
	if !report.Classes.Priced {
		t.Errorf("a known model must be priced, reason was %q", report.Classes.UnpricedReason)
	}
	vol := report.Classes.Input.VolumePercent + report.Classes.CacheWrite.VolumePercent +
		report.Classes.CacheRead.VolumePercent + report.Classes.Output.VolumePercent
	if vol != 100 {
		t.Errorf("volume shares sum to %d, want 100", vol)
	}
	cost := report.Classes.Input.CostPercent + report.Classes.CacheWrite.CostPercent +
		report.Classes.CacheRead.CostPercent + report.Classes.Output.CostPercent
	if cost != 100 {
		t.Errorf("cost shares sum to %d, want 100", cost)
	}
}

// The whole reason Task 3 extracted one resolver: the same usage on the same
// model must get the same pricing verdict from both commands. A live session and
// its own committed checkpoint disagreeing about whether cost is showable is a
// contradiction the user cannot resolve.
//
// The checkpoint side is pinned to TokenUsageVersionDelta because that is the
// version the current binary writes — a legacy checkpoint is a genuinely
// different fact (see the TTL test below), not a parity break.
func TestSessionTokensReport_Classes_AgreeWithCheckpointForSameData(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		model string
		usage *agent.TokenUsage
	}{
		{"priced", "claude-sonnet-4.6", &agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, CacheReadTokens: 6000, OutputTokens: 1000}},
		{"unknown model", "some-unknown-model", &agent.TokenUsage{InputTokens: 1000, OutputTokens: 100}},
		{"subagent on another provider", "claude-sonnet-4.6", &agent.TokenUsage{
			InputTokens: 1000, OutputTokens: 100,
			SubagentTokens: &agent.TokenUsage{Model: "gpt-5.3-codex", InputTokens: 500, OutputTokens: 50},
		}},
		{"subagent with no ratios", "claude-sonnet-4.6", &agent.TokenUsage{
			InputTokens: 1000, OutputTokens: 100,
			SubagentTokens: &agent.TokenUsage{Model: "some-unknown-model", InputTokens: 500, OutputTokens: 50},
		}},
		{"subagent in the same family", "claude-sonnet-4.6", &agent.TokenUsage{
			InputTokens: 1000, OutputTokens: 100,
			SubagentTokens: &agent.TokenUsage{Model: "claude-haiku-4.5", InputTokens: 500, OutputTokens: 50},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			live := buildSessionTokensReport(liveSessionFor(t, tc.model, tc.usage), "idle")
			committed := classesReportFor(t, "Claude Code", tc.model, checkpoint.TokenUsageVersionDelta, tc.usage)

			if live.Classes == nil || committed.Classes == nil {
				t.Fatalf("both commands must report classes: live=%v committed=%v", live.Classes, committed.Classes)
			}
			if live.Classes.Priced != committed.Classes.Priced {
				t.Errorf("priced disagrees: live=%v committed=%v", live.Classes.Priced, committed.Classes.Priced)
			}
			if live.Classes.UnpricedReason != committed.Classes.UnpricedReason {
				t.Errorf("withheld reason disagrees:\n live      = %q\n committed = %q",
					live.Classes.UnpricedReason, committed.Classes.UnpricedReason)
			}
			if live.Classes.Family != committed.Classes.Family {
				t.Errorf("family disagrees: live=%q committed=%q", live.Classes.Family, committed.Classes.Family)
			}
			if live.Classes.Total != committed.Classes.Total {
				t.Errorf("total disagrees: live=%d committed=%d", live.Classes.Total, committed.Classes.Total)
			}
		})
	}
}

// Live state is the current binary's own struct: CacheCreation1hTokens is
// written whenever the agent records it, so its absence means zero rather than
// "not recorded". The TTL reason is checkpoint-only by construction, and a live
// session must never print it — that is the one place the two commands are
// deliberately allowed to differ on the same-looking data.
func TestSessionTokensReport_Classes_NeverWithholdsForUnknownTTL(t *testing.T) {
	t.Parallel()

	usage := &agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, OutputTokens: 100}

	live := buildSessionTokensReport(liveSessionFor(t, "claude-sonnet-4.6", usage), "idle")
	if live.Classes == nil {
		t.Fatal("expected a breakdown")
	}
	if live.Classes.UnpricedReason == unpricedUnknownTTL {
		t.Error("live state always knows the TTL split; the checkpoint-only TTL reason must never appear")
	}
	if !live.Classes.Priced {
		t.Errorf("cache writes with no 1h figure mean zero on live state, so this must stay priced; got %q",
			live.Classes.UnpricedReason)
	}

	// The same bytes in a legacy checkpoint genuinely are unknowable, so the
	// divergence is a real difference in the data, not an inconsistency.
	legacy := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", 0, usage)
	if legacy.Classes.Priced {
		t.Error("a legacy checkpoint cannot know the TTL split and must stay unpriced")
	}
}

// The table has to actually reach the user, not just the struct.
func TestSessionTokensText_RendersTheBillingTable(t *testing.T) {
	t.Parallel()

	report := buildSessionTokensReport(liveSessionFor(t, "claude-sonnet-4.6",
		&agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, CacheReadTokens: 6000, OutputTokens: 1000}), "idle")

	var buf bytes.Buffer
	writeSessionTokensText(&buf, report)
	out := buf.String()

	for _, want := range []string{"How it was billed", "Fresh input", "Cache write", "Cache read", "Output", "cost"} {
		if !strings.Contains(out, want) {
			t.Errorf("session tokens output missing %q:\n%s", want, out)
		}
	}
}

// A session with no usage at all reports no table rather than four zeros, which
// would read as a free session.
func TestSessionTokensReport_Classes_AbsentWhenNoUsage(t *testing.T) {
	t.Parallel()

	report := buildSessionTokensReport(liveSessionFor(t, "claude-sonnet-4.6", nil), "idle")
	if report.Classes != nil {
		t.Errorf("no usage must yield no breakdown, got %+v", report.Classes)
	}
}
