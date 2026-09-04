package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// classesReportFor builds a one-session checkpoint report with the given
// agent, model, token-usage version and usage.
func classesReportFor(t *testing.T, agentName types.AgentType, model string, version int, usage *agent.TokenUsage) checkpointTokensReport {
	t.Helper()
	cpID := id.MustCheckpointID("abc123abc123")
	return buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID:      cpID,
			Sessions:          []checkpoint.SessionFilePaths{{Metadata: "0/metadata.json"}},
			TokenUsageVersion: version,
		},
		[]*checkpoint.Metadata{{
			SessionID:  "s1",
			Agent:      agentName,
			Model:      model,
			TokenUsage: usage,
		}},
		0,
	)
}

// The breakdown is the point of the command: a delta-scoped checkpoint from a
// model we have ratios for gets both volume and cost shares.
func TestCheckpointTokensReport_Classes_PricedWhenModelAndVersionKnown(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, CacheReadTokens: 6000, OutputTokens: 1000})

	if report.Classes == nil {
		t.Fatal("a checkpoint with usage must carry a class breakdown")
	}
	if !report.Classes.Priced {
		t.Error("a known model on a v2 checkpoint must be priced")
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
	// The whole reason for showing both: 60% of the volume is not 60% of the cost.
	if report.Classes.CacheRead.VolumePercent == report.Classes.CacheRead.CostPercent {
		t.Error("cache read's volume and cost share should diverge — that divergence is the point")
	}
}

// This is the agent-agnostic promise: an agent whose transcript can never be
// attributed still gets the full billing breakdown.
func TestCheckpointTokensReport_Classes_WorkForAnyAgent(t *testing.T) {
	t.Parallel()

	for _, agentName := range []types.AgentType{"Cursor", "Copilot CLI", "Factory AI Droid", "some-external-agent"} {
		t.Run(string(agentName), func(t *testing.T) {
			t.Parallel()
			report := classesReportFor(t, agentName, "", checkpoint.TokenUsageVersionDelta,
				&agent.TokenUsage{InputTokens: 500, CacheReadTokens: 500})
			if report.Classes == nil {
				t.Fatalf("%s must still get a class breakdown", agentName)
			}
			if report.Classes.Input.VolumePercent+report.Classes.CacheRead.VolumePercent != 100 {
				t.Error("volume shares must be exact regardless of agent")
			}
		})
	}
}

// No recorded model means no verified ratios. Volume is still exact; cost is
// absent rather than zero, which would read as "this cost nothing".
func TestCheckpointTokensReport_Classes_UnpricedWithoutModel(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{InputTokens: 1000, CacheReadTokens: 3000})

	if report.Classes == nil {
		t.Fatal("usage without a model must still produce volume shares")
	}
	if report.Classes.Priced {
		t.Error("no model means no verified ratios; the breakdown must not claim a cost")
	}
}

// A legacy row cannot tell "no 1-hour cache writes" from "TTL not recorded",
// and Anthropic bills the two TTLs differently, so cost must stay absent.
func TestCheckpointTokensReport_Classes_LegacyAnthropicIsUnpriced(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", 0,
		&agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, CacheReadTokens: 6000, OutputTokens: 1000})

	if report.Classes == nil {
		t.Fatal("a legacy checkpoint still gets volume shares")
	}
	if report.Classes.Priced {
		t.Error("an unknown cache-write TTL must not be priced at a guessed rate")
	}
}

// A provider that charges one rate for both cache TTLs has no ambiguity to
// resolve, so a legacy row is still priceable.
func TestCheckpointTokensReport_Classes_LegacySingleRateProviderIsPriced(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Codex", "gpt-5.3-codex", 0,
		&agent.TokenUsage{InputTokens: 1000, CacheReadTokens: 6000, OutputTokens: 1000})

	if report.Classes == nil {
		t.Fatal("a legacy checkpoint still gets volume shares")
	}
	if !report.Classes.Priced {
		t.Error("a provider with one cache-write rate has no TTL ambiguity; it should still be priced")
	}
}

// Mixed models across sessions cannot share one ratio row. Showing a cost that
// silently covers only some sessions is worse than showing none.
func TestCheckpointTokensReport_Classes_MixedModelsAreUnpriced(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123abc124")
	report := buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions: []checkpoint.SessionFilePaths{
				{Metadata: "0/metadata.json"},
				{Metadata: "1/metadata.json"},
			},
			TokenUsageVersion: checkpoint.TokenUsageVersionDelta,
		},
		[]*checkpoint.Metadata{
			{SessionID: "s1", Agent: "Claude Code", Model: "claude-sonnet-4.6",
				TokenUsage: &agent.TokenUsage{InputTokens: 1000, OutputTokens: 100}},
			{SessionID: "s2", Agent: "Codex", Model: "gpt-5.3-codex",
				TokenUsage: &agent.TokenUsage{InputTokens: 1000, OutputTokens: 100}},
		},
		0,
	)

	if report.Classes == nil {
		t.Fatal("a multi-session checkpoint still gets volume shares")
	}
	if report.Classes.Priced {
		t.Error("two different models cannot share one ratio row; cost must be omitted")
	}
	// Both models are individually priceable, so "no verified price ratios for
	// this model" is false. The reason must name the real case.
	if got := report.Classes.UnpricedReason; got != unpricedMixedModels {
		t.Errorf("reason = %q, want %q", got, unpricedMixedModels)
	}
}

// A checkpoint that recorded nothing must keep saying so, not render zeros.
func TestCheckpointTokensReport_Classes_AbsentWhenNoUsage(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta, nil)

	if report.Classes != nil {
		t.Error("no recorded usage must produce no breakdown at all")
	}
	if len(report.Limitations) == 0 {
		t.Error("the existing 'no token usage recorded' limitation must survive")
	}
}

// Subsets ride alongside their parent class and are never added to the total.
func TestCheckpointTokensReport_Classes_CarrySubsets(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens:           100,
			CacheCreationTokens:   400,
			CacheCreation1hTokens: 150,
			CacheReadTokens:       400,
			OutputTokens:          100,
			ThinkingTokens:        60,
		})

	if report.Classes == nil {
		t.Fatal("expected a breakdown")
	}
	if report.Classes.Total != 1000 {
		t.Errorf("Total = %d, want 1000 (subsets excluded)", report.Classes.Total)
	}
	if report.Classes.CacheWrite1h != 150 || report.Classes.Thinking != 60 {
		t.Error("subsets must be reported alongside their parent class")
	}
}

// The breakdown must render for a human, not just in --json.
func TestWriteTokenClasses_Priced(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens: 1000, CacheCreationTokens: 2000, CacheCreation1hTokens: 500,
			CacheReadTokens: 6000, OutputTokens: 1000, ThinkingTokens: 300,
		})

	var buf bytes.Buffer
	writeTokenClasses(&buf, report.Classes)
	out := buf.String()

	for _, want := range []string{"How it was billed", "Fresh input", "Cache write", "Cache read", "Output", "cost", "1h TTL", "thinking"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered breakdown missing %q\n%s", want, out)
		}
	}
}

// "<1%" means "rounds below one percent". On a family that does not bill cache
// writes at all (openai-6x/8x, the Gemini families) a cache-write class carries
// tokens whose true cost share is exactly zero, and printing "<1%" there claims
// a cost the provider never charges.
func TestWriteTokenClasses_ZeroCostClassIsNotUnderOnePercent(t *testing.T) {
	t.Parallel()

	// gpt-5.5 -> priceFamilyOpenAI6x, which defines no CacheWrite weights.
	report := classesReportFor(t, "Codex", "gpt-5.5", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens: 40000, CacheCreationTokens: 90000,
			CacheReadTokens: 200000, OutputTokens: 9000,
		})

	if !report.Classes.Priced {
		t.Fatalf("expected a priced breakdown for gpt-5.5, got reason %q", report.Classes.UnpricedReason)
	}
	if got := report.Classes.CacheWrite.Tokens; got == 0 {
		t.Fatal("test needs cache-write tokens present for the distinction to matter")
	}

	var buf bytes.Buffer
	writeTokenClasses(&buf, report.Classes)
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, "Cache write") {
			continue
		}
		if strings.Contains(line, "<1%") {
			t.Errorf("cache write costs exactly nothing on this family; row must not say \"<1%%\":\n%s", line)
		}
		if !strings.Contains(line, "0%") {
			t.Errorf("expected an explicit 0%% cost share, got:\n%s", line)
		}
	}
}

// Without a verified ratio row the cost column must not appear at all — an
// empty or zeroed column reads as "this cost nothing".
func TestWriteTokenClasses_UnpricedOmitsCostColumn(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Cursor", "", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{InputTokens: 1000, CacheReadTokens: 3000})

	var buf bytes.Buffer
	writeTokenClasses(&buf, report.Classes)
	out := buf.String()

	// Assert on the header row: a substring check for "cost" would pass merely
	// because the withheld-reason sentence starts with a capital C.
	header := strings.SplitN(out, "\n", 4)[2]
	if strings.Contains(header, "cost") {
		t.Errorf("unpriced breakdown must not show a cost column, header was %q\n%s", header, out)
	}
	if !strings.Contains(header, "volume") {
		t.Errorf("unpriced breakdown must still show volume, header was %q\n%s", header, out)
	}
	if !strings.Contains(out, "no verified price ratios") {
		t.Errorf("unpriced breakdown must say why cost is missing\n%s", out)
	}
}

// Nothing recorded renders nothing rather than an empty table.
func TestWriteTokenClasses_NilRendersNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeTokenClasses(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("nil breakdown must render nothing, got %q", buf.String())
	}
}

// Unreadable session metadata makes the report fall back to the root summary's
// total, which covers sessions whose models we never saw. Pricing that total
// against the models that happened to read would apply one provider's ratios to
// another's tokens.
func TestCheckpointTokensReport_Classes_UnreadableMetadataIsUnpriced(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123abc125")
	report := buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions: []checkpoint.SessionFilePaths{
				{Metadata: "0/metadata.json"},
				{Metadata: "1/metadata.json"},
			},
			TokenUsageVersion: checkpoint.TokenUsageVersionDelta,
			TokenUsage:        &agent.TokenUsage{InputTokens: 5000, OutputTokens: 5000},
		},
		[]*checkpoint.Metadata{
			{SessionID: "s1", Agent: "Claude Code", Model: "claude-sonnet-4.6",
				TokenUsage: &agent.TokenUsage{InputTokens: 1000, OutputTokens: 100}},
		},
		1, // the second session's metadata failed to read
	)

	if report.Classes == nil {
		t.Fatal("volume shares must still be reported")
	}
	if report.Classes.Priced {
		t.Error("a total covering sessions whose models were never read must not be priced")
	}
}

// Two sessions on the same model share one ratio row, so they stay priced —
// otherwise the mixed-model guard would quietly unprice every multi-session
// checkpoint.
func TestCheckpointTokensReport_Classes_SameModelMultiSessionStaysPriced(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123abc126")
	report := buildCheckpointTokensReport(
		cpID,
		&checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions: []checkpoint.SessionFilePaths{
				{Metadata: "0/metadata.json"},
				{Metadata: "1/metadata.json"},
			},
			TokenUsageVersion: checkpoint.TokenUsageVersionDelta,
		},
		[]*checkpoint.Metadata{
			{SessionID: "s1", Agent: "Claude Code", Model: "claude-sonnet-4.6",
				TokenUsage: &agent.TokenUsage{InputTokens: 1000, OutputTokens: 100}},
			{SessionID: "s2", Agent: "Claude Code", Model: "claude-opus-4.6",
				TokenUsage: &agent.TokenUsage{InputTokens: 1000, OutputTokens: 100}},
		},
		0,
	)

	if report.Classes == nil || !report.Classes.Priced {
		t.Error("two sessions in one price family must stay priced")
	}
}

// A nil element among the metadata must not be read past.
func TestCheckpointTokenWeights_NilMetaIsUnpriced(t *testing.T) {
	t.Parallel()

	w, reason := checkpointTokenWeights([]*checkpoint.Metadata{nil}, 0)
	if w.Family != "" {
		t.Errorf("a nil session metadata must yield no weights, got %q", w.Family)
	}
	// Unreadable metadata is not a mixed-model case; it takes the generic reason.
	if reason != "" {
		t.Errorf("reason = %q, want the generic one (empty)", reason)
	}
}

// The withheld reason must name the real cause, not default to "no ratios".
func TestWriteTokenClasses_StatesTheRealReason(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", 0,
		&agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, OutputTokens: 100})

	var buf bytes.Buffer
	writeTokenClasses(&buf, report.Classes)
	out := buf.String()

	if strings.Contains(out, "no verified price ratios") {
		t.Errorf("a legacy Anthropic checkpoint has ratios; the reason must be the TTL split\n%s", out)
	}
	if !strings.Contains(out, "TTL") {
		t.Errorf("expected the TTL reason\n%s", out)
	}
}

// #2155 records a Model per subagent entry precisely so cost can be weighted
// per model. Those tokens are flattened into the classes, so a subagent on a
// differently-priced model would otherwise be costed at its parent's rate while
// the report claims the total is priced.
func TestCheckpointTokensReport_Classes_SubagentOnAnotherProviderIsUnpriced(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Pi", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens: 1000, OutputTokens: 100,
			SubagentTokens: &agent.TokenUsage{
				Model:       "gpt-5.3-codex", // 8x output against the parent's 5x
				InputTokens: 5000, OutputTokens: 900,
			},
		})

	if report.Classes == nil {
		t.Fatal("volume shares must still be reported")
	}
	if report.Classes.Priced {
		t.Error("a subagent priced by another provider's ratios must unprice the total")
	}
}

// A subagent on the same price family is fine — all Claude models share one
// Anthropic row, so this must not unprice every subagent-bearing checkpoint.
func TestCheckpointTokensReport_Classes_SubagentInSameFamilyStaysPriced(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens: 1000, OutputTokens: 100,
			SubagentTokens: &agent.TokenUsage{Model: "claude-haiku-4.5", InputTokens: 5000, OutputTokens: 900},
		})

	if report.Classes == nil || !report.Classes.Priced {
		t.Error("a subagent in the parent's price family must stay priced")
	}
}

// Subagent entries usually record no model at all. Unpricing on that would gut
// the feature for the common case, so the parent's family is inherited.
func TestCheckpointTokensReport_Classes_SubagentWithoutModelInheritsParent(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens: 1000, OutputTokens: 100,
			SubagentTokens: &agent.TokenUsage{InputTokens: 5000, OutputTokens: 900},
		})

	if report.Classes == nil || !report.Classes.Priced {
		t.Error("an unrecorded subagent model must inherit the parent's family, not unprice")
	}
}

// Both blocks in one report must agree. The "Token usage" line has always set
// Total from totalTokens, which recurses into subagents, while its class fields
// came from the top level only — so its own parts did not sum to its own total.
// Rendering the class breakdown beside it exposed that as two contradictory
// lists under the same labels on a real checkpoint.
func TestCheckpointTokensReport_UsageAndClassesAgree(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens: 1000, CacheCreationTokens: 2000, CacheReadTokens: 6000, OutputTokens: 1000,
			SubagentTokens: &agent.TokenUsage{
				InputTokens: 500, CacheCreationTokens: 500, CacheReadTokens: 3000, OutputTokens: 500,
			},
		})

	if report.Tokens == nil || report.Classes == nil {
		t.Fatal("expected both blocks")
	}

	// The older line's parts must sum to its own total.
	parts := report.Tokens.Input + report.Tokens.CacheRead + report.Tokens.CacheWrite + report.Tokens.Output
	if parts != report.Tokens.Total {
		t.Errorf("Token usage parts sum to %d but its Total is %d", parts, report.Tokens.Total)
	}

	// And the two blocks must not print different numbers under the same label.
	for _, c := range []struct {
		label     string
		line, cls int
	}{
		{"input", report.Tokens.Input, report.Classes.Input.Tokens},
		{"cache write", report.Tokens.CacheWrite, report.Classes.CacheWrite.Tokens},
		{"cache read", report.Tokens.CacheRead, report.Classes.CacheRead.Tokens},
		{"output", report.Tokens.Output, report.Classes.Output.Tokens},
		{"total", report.Tokens.Total, report.Classes.Total},
	} {
		if c.line != c.cls {
			t.Errorf("%s: Token usage says %d, the breakdown says %d — one report, two answers", c.label, c.line, c.cls)
		}
	}

	// Subagent usage stays visible as its own figure.
	if report.Tokens.SubagentTotal != 4500 {
		t.Errorf("SubagentTotal = %d, want 4500", report.Tokens.SubagentTotal)
	}
}

// A class holding real tokens must not print "0%" — that reads as broken next
// to a six-figure token count. An empty class still prints "0%".
func TestFormatSharePercent(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		tokens, percent int
		want            string
	}{
		{274800, 0, "<1%"},
		{0, 0, "0%"},
		{1000, 7, "7%"},
		{9999999, 93, "93%"},
	} {
		if got := formatSharePercent(tt.tokens, tt.percent); got != tt.want {
			t.Errorf("formatSharePercent(%d, %d) = %q, want %q", tt.tokens, tt.percent, got, tt.want)
		}
	}
}
