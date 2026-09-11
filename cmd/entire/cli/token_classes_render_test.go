package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

// Tests for token_classes_render.go — the block both `checkpoint tokens` and
// `session tokens` render. Task 1 left these in the checkpoint command's test
// file to keep a zero-behaviour-change refactor free of churn; the renderer now
// has two callers and its own file, so its tests follow it. The checkpoint
// report and weights tests deliberately stayed behind: they test the report,
// not the renderer.

// The breakdown must render for a human, not just in --json.
func TestWriteTokenClasses_Priced(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", checkpoint.TokenUsageVersionDelta,
		&agent.TokenUsage{
			InputTokens: 1000, CacheCreationTokens: 2000, CacheCreation1hTokens: 500,
			CacheReadTokens: 6000, OutputTokens: 1000, ThinkingTokens: 300,
		})

	var buf bytes.Buffer
	writeTokenClasses(&buf, report.Classes, 0)
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
	writeTokenClasses(&buf, report.Classes, 0)
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
	writeTokenClasses(&buf, report.Classes, 0)
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
	if !strings.Contains(out, unpricedNoModel) {
		t.Errorf("unpriced breakdown must say why cost is missing\n%s", out)
	}
}

// The withheld reason must name the real cause, not default to "no ratios".
func TestWriteTokenClasses_StatesTheRealReason(t *testing.T) {
	t.Parallel()

	report := classesReportFor(t, "Claude Code", "claude-sonnet-4.6", 0,
		&agent.TokenUsage{InputTokens: 1000, CacheCreationTokens: 2000, OutputTokens: 100})

	var buf bytes.Buffer
	writeTokenClasses(&buf, report.Classes, 0)
	out := buf.String()

	if strings.Contains(out, unpricedNoModel) {
		t.Errorf("a legacy Anthropic checkpoint has ratios; the reason must be the TTL split\n%s", out)
	}
	if !strings.Contains(out, "TTL") {
		t.Errorf("expected the TTL reason\n%s", out)
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
		{"subagent with no ratios", unpricedSomeTokensNoRatios, unpricedSomeTokensNoRatios, false},
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
			writeTokenClasses(&buf, classes, 0)
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

func TestWriteTokenClasses_SubagentShareShownWhenNonZero(t *testing.T) {
	t.Parallel()

	classes := &tokenClassBreakdown{
		Input:     tokenClassShare{Tokens: 42000, VolumePercent: 10},
		CacheRead: tokenClassShare{Tokens: 240000, VolumePercent: 58},
		Total:     411000,
	}

	var buf bytes.Buffer
	writeTokenClasses(&buf, classes, 54000)
	out := buf.String()

	if !strings.Contains(out, "Of the total, subagents used") {
		t.Errorf("expected the subagent line, got:\n%s", out)
	}
	if !strings.Contains(out, "54k") {
		t.Errorf("expected 54k (formatTokenCount trims the .0), got:\n%s", out)
	}
	if !strings.Contains(out, "13%") {
		t.Errorf("54000/411000 rounds to 13%%, got:\n%s", out)
	}
}

// Absent subagent tokens cannot distinguish "none spawned" from "spawned but
// not captured" in a metadata-only layer, so claiming either is unprovable —
// and it would be noise on the majority of sessions that spawned none.
func TestWriteTokenClasses_SubagentShareSilentWhenZero(t *testing.T) {
	t.Parallel()

	classes := &tokenClassBreakdown{Input: tokenClassShare{Tokens: 1000, VolumePercent: 100}, Total: 1000}

	var buf bytes.Buffer
	writeTokenClasses(&buf, classes, 0)

	if strings.Contains(buf.String(), "subagents") {
		t.Errorf("zero subagent tokens must print nothing about subagents, got:\n%s", buf.String())
	}
}

// Nothing recorded renders nothing rather than an empty table.
func TestWriteTokenClasses_NilRendersNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeTokenClasses(&buf, nil, 0)
	if buf.Len() != 0 {
		t.Errorf("nil breakdown must render nothing, got %q", buf.String())
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

// The subagent figure and the class figures come from walks with different
// bounds: the classes are flattened at types.MaxSubagentDepth, while
// SubagentTotal comes from totalTokens, which is unbounded (deliberately still
// unbounded — bounding it is on the plan's known-bugs list). On a chain deeper
// than the bound the subagent figure can therefore exceed the Total printed
// directly above it, and "of the total, subagents used" more than the total is
// a report contradicting itself. Print nothing rather than a share that cannot
// be true.
func TestWriteTokenClasses_SubagentShareSilentWhenItExceedsTheTotal(t *testing.T) {
	t.Parallel()

	classes := &tokenClassBreakdown{
		Input: tokenClassShare{Tokens: 1000, VolumePercent: 100},
		Total: 1000,
	}

	var buf bytes.Buffer
	writeTokenClasses(&buf, classes, 1500)
	out := buf.String()

	if strings.Contains(out, "subagents used") {
		t.Errorf("a subagent figure larger than the total is not a share of it; print nothing:\n%s", out)
	}
}
