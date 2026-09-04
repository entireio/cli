package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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
	if reason != unpricedSomeTokensNoRatios {
		t.Errorf("reason = %q, want %q", reason, unpricedSomeTokensNoRatios)
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

func TestSessionTokenTTLKnown_IsTrueForLiveState(t *testing.T) {
	t.Parallel()

	if !sessionTokenTTLKnown() {
		t.Error("live state is written by the running binary, which records the 1h split whenever the agent reports it, so absence means zero")
	}
}

// The point of the feature: a live session on a model we have ratios for gets
// the same volume-and-cost table `checkpoint tokens` shows.
func TestBuildSessionTokensReport_PricedClassesForKnownModel(t *testing.T) {
	t.Parallel()

	state := &strategy.SessionState{
		SessionID: "live-priced",
		AgentType: "Claude Code",
		ModelName: "claude-sonnet-4.6",
		TokenUsage: &agent.TokenUsage{
			InputTokens: 42000, CacheCreationTokens: 118000, CacheCreation1hTokens: 22000,
			CacheReadTokens: 240000, OutputTokens: 11000, ThinkingTokens: 4000, APICallCount: 37,
		},
	}

	report := buildSessionTokensReport(state, "active")

	if report.Classes == nil {
		t.Fatal("a live session with usage must get a breakdown")
	}
	if !report.Classes.Priced {
		t.Fatalf("claude-sonnet-4.6 has verified ratios; reason was %q", report.Classes.UnpricedReason)
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

// A model-less session still gets volume shares; only cost is withheld.
func TestBuildSessionTokensReport_UnpricedWithoutModel(t *testing.T) {
	t.Parallel()

	state := &strategy.SessionState{
		SessionID:  "live-nomodel",
		AgentType:  "Cursor",
		TokenUsage: &agent.TokenUsage{InputTokens: 1000, CacheReadTokens: 3000},
	}

	report := buildSessionTokensReport(state, "active")

	if report.Classes == nil {
		t.Fatal("a model-less session still gets volume shares")
	}
	if report.Classes.Priced {
		t.Error("no model means no verified ratios; cost must be withheld")
	}
	if report.Classes.UnpricedReason != unpricedNoModel {
		t.Errorf("reason = %q, want %q", report.Classes.UnpricedReason, unpricedNoModel)
	}
}

// A session with no usage at all reports no table rather than four zeros, which
// would read as a free session.
func TestBuildSessionTokensReport_NoClassesWithoutUsage(t *testing.T) {
	t.Parallel()

	report := buildSessionTokensReport(&strategy.SessionState{SessionID: "empty", AgentType: "Cursor"}, "active")
	if report.Classes != nil {
		t.Error("no recorded usage must produce no breakdown")
	}
}

// A subagent on another provider unprices, at any depth the walk reaches.
func TestSubagentPricingReason_CatchesAnotherProvider(t *testing.T) {
	t.Parallel()

	usage := &agent.TokenUsage{
		InputTokens: 1000, OutputTokens: 100,
		SubagentTokens: &agent.TokenUsage{Model: "gpt-5.3-codex", InputTokens: 500, OutputTokens: 50},
	}

	if weights, _ := tokenWeightsForSession("claude-sonnet-4.6", usage); weights.Family != "" {
		t.Error("a subagent inside the depth bound is counted in the classes and must still unprice")
	}
}

// checkpointAgentBriefSessionReport bridges a checkpoint report into the shared
// brief helpers. Nothing in the brief reads Classes today, so a dropped field is
// invisible — which is exactly why it should be carried: the day someone makes
// --agent-brief class-aware, the checkpoint brief would silently see nil while
// the session brief saw real data.
func TestCheckpointAgentBriefSessionReport_CarriesClasses(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{InputTokens: 1000, CacheReadTokens: 6000, OutputTokens: 1000})
	if report.Classes == nil {
		t.Fatal("fixture must produce classes")
	}

	if bridged := checkpointAgentBriefSessionReport(report); bridged.Classes != report.Classes {
		t.Error("the bridge must carry Classes; dropping it is invisible until the brief becomes class-aware")
	}
}

// Absent subagent tokens cannot distinguish "none spawned" from "spawned but
// not captured" in a metadata-only layer, so claiming either is unprovable —
// and it would be noise on the majority of sessions that spawned none.

// The figure appears once. It used to be a "Likely contributors" entry as well.
func TestSessionTokens_SubagentFigureAppearsOnlyOnce(t *testing.T) {
	t.Parallel()

	state := &strategy.SessionState{
		// The ID deliberately avoids the substring being counted below: the
		// plan's own fixture used "live-subagents", which the Count picked up
		// off the "Session:" line and reported as a duplicate figure.
		SessionID: "live-with-helpers",
		AgentType: "Claude Code",
		ModelName: "claude-sonnet-4.6",
		TokenUsage: &agent.TokenUsage{
			InputTokens: 42000, CacheReadTokens: 240000, OutputTokens: 11000,
			SubagentTokens: &agent.TokenUsage{InputTokens: 30000, OutputTokens: 24000},
		},
	}

	var buf bytes.Buffer
	writeSessionTokensText(&buf, buildSessionTokensReport(state, "active"))
	out := buf.String()

	// "ubagents" (plural) and not "ubagent": with this fixture the
	// subagent-heavy recommendation also fires and says "Scope subagent tasks
	// tightly…" — singular. Matching the plural counts the figure's labels only.
	// Do not "fix" this to "ubagent"; it will start counting the advice line.
	if n := strings.Count(out, "ubagents"); n != 1 {
		t.Errorf("the subagent figure must appear exactly once in the text, found %d mentions:\n%s", n, out)
	}
}

// Decision 3: the subagents contributor stays in the report for --json even
// though the text now renders the figure inside the billed block. Removing it
// would delete an element from an array PR 1 shipped.
func TestBuildSessionTokensReport_KeepsSubagentContributorForJSON(t *testing.T) {
	t.Parallel()

	state := &strategy.SessionState{
		SessionID: "json-contributor", AgentType: "Claude Code",
		TokenUsage: &agent.TokenUsage{
			InputTokens:    1000,
			SubagentTokens: &agent.TokenUsage{InputTokens: 500},
		},
	}

	report := buildSessionTokensReport(state, "active")
	for _, c := range report.Contributors {
		if c.Kind == "subagents" {
			return
		}
	}
	t.Error("the subagents contributor must stay in the report; --json consumers read it")
}

// The "Likely contributors" section must not print a header with nothing under
// it. Both silent kinds are present here — a subagents entry (its figure now
// lives in the billed block) and a context_pressure entry with no context
// block — which is exactly the combination the old len(contributors) > 0 guard
// let through.
func TestWriteTokenContributors_NoBareHeaderWhenEverythingIsSilent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeTokenContributors(&buf, []sessionTokensContributor{
		{Kind: "subagents", Label: "Subagents", Tokens: 54000},
		{Kind: "context_pressure", Label: "Context pressure"},
	}, nil)

	if buf.Len() != 0 {
		t.Errorf("contributors that all render nothing must print no section at all, got:\n%s", buf.String())
	}
}

// …but a section with anything real to say still renders.
func TestWriteTokenContributors_RendersWhenSomethingIsVisible(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeTokenContributors(&buf, []sessionTokensContributor{
		{Kind: "subagents", Label: "Subagents", Tokens: 54000},
		{Kind: "skills", Label: "Skills/slash commands: brainstorm"},
	}, nil)

	out := buf.String()
	if !strings.Contains(out, "Likely contributors") || !strings.Contains(out, "brainstorm") {
		t.Errorf("a visible contributor must still render its section, got:\n%s", out)
	}
	if strings.Contains(out, "Subagents") {
		t.Errorf("the subagents figure moved into the billed block, got:\n%s", out)
	}
}

func TestSessionTokensDuration_UsesInteractionSpan(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-6 * time.Hour)
	last := start.Add(2*time.Hour + 14*time.Minute)
	state := &strategy.SessionState{
		SessionID:           "live-duration",
		AgentType:           "Claude Code",
		StartedAt:           start,
		LastInteractionTime: &last,
		TokenUsage:          &agent.TokenUsage{InputTokens: 100},
	}

	// 6h elapsed, 2h14m of interaction: a token report is about work done.
	if got := sessionTokensDuration(state); got != "2h 14m so far" {
		t.Errorf("duration = %q, want %q", got, "2h 14m so far")
	}
}

func TestSessionTokensDuration_EmptyWithoutInteraction(t *testing.T) {
	t.Parallel()

	state := &strategy.SessionState{
		SessionID:  "live-nointeraction",
		AgentType:  "Claude Code",
		StartedAt:  time.Now().Add(-3 * time.Hour),
		TokenUsage: &agent.TokenUsage{InputTokens: 100},
	}

	// Not "0m", and not the 3h of elapsed time — neither is a measure of work.
	if got := sessionTokensDuration(state); got != "" {
		t.Errorf("duration = %q, want empty when no interaction was recorded", got)
	}
}

func TestFormatDurationShort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   time.Duration
		want string
	}{
		{2*time.Hour + 14*time.Minute, "2h 14m"},
		{14 * time.Minute, "14m"},
		{45 * time.Second, "45s"},
		// A whole number of hours drops the minutes rather than saying "3h 0m".
		{3 * time.Hour, "3h"},
		// Seconds are noise once minutes are on the clock.
		{time.Hour + 30*time.Minute + 20*time.Second, "1h 30m"},
		{5*time.Minute + 40*time.Second, "5m"},
	}
	for _, tc := range cases {
		if got := formatDurationShort(tc.in); got != tc.want {
			t.Errorf("formatDurationShort(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The line reaches the user, and says "so far" because the session is live.
func TestSessionTokensText_RendersDuration(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-4 * time.Hour)
	last := start.Add(90 * time.Minute)
	state := &strategy.SessionState{
		SessionID:           "live-duration-text",
		AgentType:           "Claude Code",
		ModelName:           "claude-sonnet-4.6",
		StartedAt:           start,
		LastInteractionTime: &last,
		TokenUsage:          &agent.TokenUsage{InputTokens: 1000, OutputTokens: 100},
	}

	var buf bytes.Buffer
	writeSessionTokensText(&buf, buildSessionTokensReport(state, "active"))
	if out := buf.String(); !strings.Contains(out, "Duration: 1h 30m so far") {
		t.Errorf("expected the duration line, got:\n%s", out)
	}
}

// PR 1's lesson: its headline cost column had never rendered outside a unit
// test until it was driven by hand. This asserts the acceptance criterion on
// what a user actually sees, by parsing the percentages back out of the
// rendered table.
//
// No t.Parallel(): setupStopTestRepo calls t.Chdir.
func TestSessionTokensCmd_RendersBilledBlockFromLiveState(t *testing.T) {
	setupStopTestRepo(t)

	state := makeSessionState("cmd-billed", session.PhaseActive)
	state.AgentType = testAgentClaude
	state.ModelName = "claude-sonnet-4.6"
	state.TokenUsage = &agent.TokenUsage{
		InputTokens: 42000, CacheCreationTokens: 118000, CacheCreation1hTokens: 22000,
		CacheReadTokens: 240000, OutputTokens: 11000, ThinkingTokens: 4000, APICallCount: 37,
	}
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"cmd-billed"}) // positional: Use is "tokens [session-id]"; there is no --session flag
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "How it was billed") {
		t.Fatalf("expected the billed breakdown, got:\n%s", out)
	}
	volume, cost := parseBilledShares(t, out)
	if volume != 100 {
		t.Errorf("volume shares sum to %d%%, want 100%%\n%s", volume, out)
	}
	if cost != 100 {
		t.Errorf("cost shares sum to %d%%, want 100%%\n%s", cost, out)
	}
}

// A turn that ends in under a second is a real case (a one-line prompt), and
// "0s so far" states a duration of zero for a session that was worked on —
// the same false claim as the "0m" the empty-string rule exists to avoid.
// Found by running the command for real; every other duration test here uses a
// synthetic multi-hour span.
func TestSessionTokensDuration_EmptyForSubSecondSpan(t *testing.T) {
	t.Parallel()

	start := time.Now()
	last := start.Add(400 * time.Millisecond)
	state := &strategy.SessionState{
		SessionID:           "live-instant",
		AgentType:           "Claude Code",
		StartedAt:           start,
		LastInteractionTime: &last,
	}

	if got := sessionTokensDuration(state); got != "" {
		t.Errorf("duration = %q, want empty: a sub-second span has no duration worth stating", got)
	}
}
