package cli

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// Figure extraction mirrors tokenreport/recommend_test.go's extractFigures:
// durations first, then numbers with an optional k/M/% suffix; names in
// backticks are not figures.
var (
	tokenTestDurationFigures = regexp.MustCompile(`\d+d \d+h|\d+h \d+m|\d+m\b|\d+s\b`)
	tokenTestNumericFigures  = regexp.MustCompile(`<1%|\d[\d.,]*[kM%]?`)
	tokenTestBacktickSpans   = regexp.MustCompile("`[^`]*`")
)

// assertRecommendationFiguresVisible asserts that every figure quoted in the
// Recommendations section of a rendered report appears elsewhere in it.
func assertRecommendationFiguresVisible(t *testing.T, out string) {
	t.Helper()
	start := strings.Index(out, "\nRecommendations\n")
	if start < 0 {
		return
	}
	end := strings.Index(out[start:], "\nNotes\n")
	var recs string
	if end < 0 {
		recs = out[start:]
		out = out[:start]
	} else {
		recs = out[start : start+end]
		out = out[:start] + out[start+end:]
	}
	recs = tokenTestBacktickSpans.ReplaceAllString(recs, " ")
	figures := tokenTestDurationFigures.FindAllString(recs, -1)
	recs = tokenTestDurationFigures.ReplaceAllString(recs, " ")
	for _, f := range tokenTestNumericFigures.FindAllString(recs, -1) {
		figures = append(figures, strings.TrimRight(f, ".,"))
	}
	for _, f := range figures {
		if !strings.Contains(out, f) {
			t.Errorf("recommendation figure %q is not printed elsewhere in the report:\n%s", f, out)
		}
	}
}

// contributor builds a priced tool row.
func contributor(kind tokenreport.ContributorKind, label, skill string, volume int, share float64, details ...tokenreport.Detail) tokenreport.Contributor {
	return tokenreport.Contributor{Kind: kind, Label: label, Skill: skill, Usage: types.TokenUsage{InputTokens: volume, APICallCount: 1}, CostShare: share, Source: tokenreport.SourceTranscript, Details: details}
}

// wideAttributed has eight rows so the cutoffs bite; row index 4 and the
// last row carry details.
func wideAttributed() tokenreport.Attributed {
	rows := []tokenreport.Contributor{
		contributor(tokenreport.KindReplay, tokenreport.LabelContextReplay, "", 3_700_000, 0.30),
		contributor(tokenreport.KindTool, "Bash", "systematic-debugging", 164_600, 0.25, tokenreport.Detail{Detail: "go test ./cmd/entire/...", Calls: 9, Tokens: 140_200, CostShare: 0.20}),
		contributor(tokenreport.KindText, tokenreport.LabelAssistantText, "", 222_000, 0.15),
		contributor(tokenreport.KindSkill, "systematic-debugging", "", 41_200, 0.10),
		contributor(tokenreport.KindTool, "Read", "", 30_000, 0.08,
			tokenreport.Detail{Detail: "cmd/entire/cli/status.go", Calls: 3, Tokens: 20_000, CostShare: 0.05},
			tokenreport.Detail{Detail: "cmd/entire/cli/explain.go", Calls: 1, Tokens: 10_000, CostShare: 0.03}),
		contributor(tokenreport.KindPrompt, tokenreport.LabelPromptContext, "", 40_300, 0.06),
		contributor(tokenreport.KindTool, "Grep", "", 5_000, 0.04),
		contributor(tokenreport.KindSubagent, "Explore", "", 4_000, 0.02, tokenreport.Detail{Detail: "Explore", Calls: 2, Tokens: 4_000, CostShare: 0.02}),
	}
	return tokenreport.Attributed{Contributors: rows, PricedUnits: 1000}
}

func claudeView(attributed tokenreport.Attributed, recs ...tokenreport.Recommendation) tokenReportView {
	usage := types.TokenUsage{InputTokens: 6_500, CacheCreationTokens: 332_900, CacheCreation1hTokens: 332_900, CacheReadTokens: 3_700_000, OutputTokens: 115_100, ThinkingTokens: 65_300, APICallCount: 43}
	w, _, _ := tokenreport.WeightsFor("claude-fable-5")
	return tokenReportView{
		Report: tokenreport.Report{
			Agent: agent.AgentTypeClaudeCode, Profile: tokenreport.ProfileFor(agent.AgentTypeClaudeCode), Model: "claude-fable-5", Effort: checkpointTokensFixtureEffort,
			Usage: usage, Cost: tokenreport.ComputeCostShares(&usage, w), Attributed: attributed, Duration: 9*time.Hour + 42*time.Minute, Calls: 43,
		},
		HasUsage: true, EffortCalls: 43, Attributed: true, Recommendations: recs,
		Subagent: types.TokenUsage{InputTokens: 4_000},
	}
}

func renderBody(v *tokenReportView) string {
	var b strings.Builder
	writeTokenReportBody(&b, v)
	return b.String()
}

func TestWriteTokenWhereItWent_CutoffsAndCitations(t *testing.T) {
	t.Parallel()

	rec := tokenreport.Recommendation{
		Kind: tokenreport.RecommendationKindSession, Cause: tokenreport.CauseSubagentModel,
		Text: "Explore subagents ran 2 times on `claude-fable-5` (4k tokens, 2% of cost); delegated work like this often runs well on a smaller model.",
		Cited: []tokenreport.Citation{
			{Kind: tokenreport.KindSubagent, Label: "Explore", Detail: "Explore"},
			{Kind: tokenreport.KindTool, Label: "Read", Detail: "cmd/entire/cli/explain.go"},
		},
	}
	v := claudeView(wideAttributed(), rec)
	out := renderBody(&v)

	assertContainsAll(t, out,
		"Where it went",
		"Context replay (cache read)",
		"Bash · during systematic-debugging",
		"go test ./cmd/entire/...",
		"9 calls",
		"Skill: systematic-debugging (loaded)",
		"Read",
		"cmd/entire/cli/explain.go",
		"Prompt & system context",
		"Subagent: Explore",
		"2 calls",
		"(1 smaller item omitted)",
	)
	// Row 5 (index 4) is inside MaxRenderedRows but past MaxRenderedDetails:
	// only its cited detail prints.
	if strings.Contains(out, "cmd/entire/cli/status.go") {
		t.Errorf("uncited detail below the detail cutoff must not print:\n%s", out)
	}
	// Grep (index 6) is past the row cutoff and not cited: omitted.
	if strings.Contains(out, "  Grep") {
		t.Errorf("uncited row below the cutoff must not print:\n%s", out)
	}
	// The cited subagent row prints after the top block and before the omitted line.
	promptIdx, exploreIdx, omittedIdx := strings.Index(out, "Prompt & system context"), strings.Index(out, "Subagent: Explore"), strings.Index(out, "smaller item omitted")
	if promptIdx >= exploreIdx || exploreIdx >= omittedIdx {
		t.Errorf("cited row must follow the top block and precede the omitted line (prompt=%d explore=%d omitted=%d):\n%s", promptIdx, exploreIdx, omittedIdx, out)
	}
	assertRecommendationFiguresVisible(t, out)
}

func TestWriteTokenWhereItWent_TopRowsPrintAllDetails(t *testing.T) {
	t.Parallel()

	a := wideAttributed()
	a.Contributors = a.Contributors[:3]
	v := claudeView(a)
	out := renderBody(&v)
	assertContainsAll(t, out, "go test ./cmd/entire/...", "9 calls", "140.2k", "20%")
	if strings.Contains(out, "omitted") {
		t.Errorf("nothing omitted with three rows:\n%s", out)
	}
}

func TestWriteTokenReportBody_UsageTable(t *testing.T) {
	t.Parallel()

	v := claudeView(wideAttributed())
	out := renderBody(&v)
	assertContainsAll(t, out,
		"Usage",
		"est. cost share",
		"Input (fresh)",
		"6.5k",
		"Cache write, 1-hour",
		"332.9k",
		"(2×)",
		"Cache read",
		"3.7M",
		"(0.1×)",
		"Output",
		"115.1k",
		"(5×)",
		"of which thinking",
		"65.3k",
		"57% of output",
		"Total",
		"4.2M",
		"of which subagents",
		"4k",
		"Cost shares use Anthropic list-price ratios",
	)
	// Total is Σ of the four classes: 6.5k + 332.9k + 3.7M + 115.1k = 4,154,500.
	if got := tokenVolume(&v.Report.Usage); got != 4_154_500 {
		t.Errorf("volume = %d", got)
	}
	// The "tokens" header ends in the same column as the values under it.
	headerEnd, valueEnd := -1, -1
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Usage") {
			headerEnd = utf8.RuneCountInString(line[:strings.Index(line, "tokens")+len("tokens")])
		}
		if strings.Contains(line, "Input (fresh)") {
			valueEnd = utf8.RuneCountInString(line[:strings.Index(line, "6.5k")+len("6.5k")])
		}
	}
	if headerEnd < 0 || headerEnd != valueEnd {
		t.Errorf("tokens header ends at column %d, values at %d:\n%s", headerEnd, valueEnd, out)
	}
}

func TestThinkingLineOmitsShareWhenZero(t *testing.T) {
	t.Parallel()

	v := claudeView(wideAttributed())
	v.Report.Usage.ThinkingTokens = 0
	line := thinkingLine(&v)
	if line.tokens != "0" || line.share != "" || line.note != "" {
		t.Errorf("zero thinking line = %+v, want tokens 0 with no share or note", line)
	}
}

func TestWriteTokenNotesWrapsWithHangingIndent(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writeTokenNotes(&b, []string{strings.Repeat("note ", 30), "short"})
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if lines[0] != "" || lines[1] != "Notes" || !strings.HasPrefix(lines[2], "  - note") || !strings.HasPrefix(lines[3], "    note") {
		t.Errorf("unexpected notes layout:\n%s", b.String())
	}
	for _, line := range lines {
		if len(line) > tokenRecommendationWrap {
			t.Errorf("line longer than %d: %q", tokenRecommendationWrap, line)
		}
	}
	if lines[len(lines)-1] != "  - short" {
		t.Errorf("last line = %q", lines[len(lines)-1])
	}
}

func TestTokenReportNotes_UnrecordedModelAndAgent(t *testing.T) {
	t.Parallel()

	v := tokenReportView{Report: tokenreport.Report{Usage: types.TokenUsage{InputTokens: 10, APICallCount: 1}}, HasUsage: true}
	v.Report.Profile = tokenreport.ProfileFor(v.Report.Agent)
	notes := strings.Join(tokenReportNotes(&v), "\n")
	assertContainsAll(t, notes, "model not recorded; cost shares not estimated", "agent not recorded; totals shown, breakdown unavailable.")
}

func TestDetailCallCount(t *testing.T) {
	t.Parallel()

	if got := detailCallCount(0); got != "" {
		t.Errorf("detailCallCount(0) = %q, want empty", got)
	}
	if got := detailCallCount(1); got != "1 call" {
		t.Errorf("detailCallCount(1) = %q", got)
	}
}

func TestWriteTokenReportBody_UnpricedModelPrintsDashes(t *testing.T) {
	t.Parallel()

	a := wideAttributed()
	a.PricedUnits = 0
	a.Unpriced = []string{"mystery-model"}
	for i := range a.Contributors {
		a.Contributors[i].CostShare = 0
	}
	v := claudeView(a)
	v.Report.Model = "mystery-model"
	v.Report.Cost = tokenreport.CostShares{}
	out := renderBody(&v)

	if strings.Count(out, tokenTableUnpriced) < 8 {
		t.Errorf("expected — in every share cell, got:\n%s", out)
	}
	assertContainsAll(t, out, "no verified price ratios for `mystery-model`; its tokens count toward volume only")
	if strings.Contains(out, "list-price ratios") || strings.Contains(out, "(5×)") {
		t.Errorf("no ratio note or ratios when nothing is priced:\n%s", out)
	}
}

func TestWriteTokenReportBody_ZeroTokenToolRowAndNoUsage(t *testing.T) {
	t.Parallel()

	a := tokenreport.Attributed{Contributors: []tokenreport.Contributor{
		{Kind: tokenreport.KindTool, Label: "Skill", Source: tokenreport.SourceTranscript},
		{Kind: tokenreport.KindSubagent, Label: tokenreport.LabelUnknownSubagent, Source: tokenreport.SourceTaskRecord, Usage: types.TokenUsage{OutputTokens: 5}},
		{Kind: tokenreport.KindTool, Label: tokenreport.LabelEarlierResults, Source: tokenreport.SourceTranscript, Usage: types.TokenUsage{InputTokens: 7}},
	}, PricedUnits: 10}
	v := claudeView(a)
	v.UnknownUsageCalls = 1
	out := renderBody(&v)
	assertContainsAll(t, out, "Skill", tokenUsageNotRecorded, "Subagent: (unknown)", "Earlier tool results", "1 call with no usage recorded")

	empty := tokenReportView{Report: tokenreport.Report{Agent: agent.AgentTypeClaudeCode, Profile: tokenreport.ProfileFor(agent.AgentTypeClaudeCode)}}
	out = renderBody(&empty)
	assertContainsAll(t, out, "Usage", "Token usage: not recorded")
	if strings.Contains(out, "Where it went") || strings.Contains(out, "Notes") {
		t.Errorf("nothing else without usage:\n%s", out)
	}
}

func TestWriteTokenRecommendationSentences_WrapsAndSkipsWhenEmpty(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writeTokenRecommendationSentences(&b, nil)
	if b.Len() != 0 {
		t.Errorf("expected nothing for no recommendations, got %q", b.String())
	}
	long := strings.Repeat("word ", 40)
	writeTokenRecommendationSentences(&b, []tokenreport.Recommendation{{Text: long}, {Text: "Second."}})
	for _, line := range strings.Split(b.String(), "\n") {
		if len(line) > tokenRecommendationWrap {
			t.Errorf("line longer than %d: %q", tokenRecommendationWrap, line)
		}
	}
	assertContainsAll(t, b.String(), "Recommendations\n  word", "\n\n  Second.")
}

func TestWriteTokenAgentBrief(t *testing.T) {
	t.Parallel()

	rec := tokenreport.Recommendation{Kind: tokenreport.RecommendationKindSession, Cause: tokenreport.CauseLongSession, Text: "This session ran 9h 42m. Splitting work this long into several shorter sessions would have cost less."}
	v := claudeView(wideAttributed(), rec, tokenreport.Recommendation{Cause: tokenreport.CauseThinking, Text: "Thinking took 57% of output tokens."})
	var b strings.Builder
	writeTokenAgentBrief(&b, "Checkpoint token brief", "Checkpoint", "abc", &v)
	assertContainsAll(t, b.String(),
		"Checkpoint token brief\nCheckpoint: abc\n",
		"Token usage: 4.2M total; 43 API calls; 9h 42m; cache read",
		"of cost.",
		"Next best action:\nThis session ran 9h 42m.",
		"Signals:\n- long_session\n- thinking\n",
	)
	if strings.Contains(b.String(), "Pattern:") {
		t.Errorf("no Pattern block in B1:\n%s", b.String())
	}

	b.Reset()
	quiet := claudeView(wideAttributed())
	writeTokenAgentBrief(&b, "Checkpoint token brief", "Checkpoint", "abc", &quiet)
	assertContainsAll(t, b.String(), "Continue normally; no token recommendation fired for this report.", "- none: no token recommendation fired")
}

func TestTokenReportNotes_ProfileAndMixedFamilies(t *testing.T) {
	t.Parallel()

	v := claudeView(wideAttributed())
	v.Report.Agent = agent.AgentTypeFactoryAIDroid
	v.Report.Profile = tokenreport.ProfileFor(agent.AgentTypeFactoryAIDroid)
	v.Limitations = []string{"custom note"}
	notes := tokenReportNotes(&v)
	assertContainsAll(t, strings.Join(notes, "\n"), "no verified capability profile for Factory AI Droid; totals shown, breakdown not verified.", "custom note")
	// The caller's Limitations explain the totals and so come first; the
	// pricing note and the profile caveat follow in that order.
	if len(notes) < 3 || notes[0] != "custom note" || !strings.HasPrefix(notes[1], "Cost shares use Anthropic list-price ratios") || !strings.HasPrefix(notes[2], "no verified capability profile") {
		t.Errorf("notes order = %q, want limitations, pricing, profile", notes)
	}

	if got := tokenReportNotes(&tokenReportView{}); got != nil {
		t.Errorf("a view with nothing to say has nil notes, got %q", got)
	}

	mixed := claudeView(wideAttributed())
	mixed.Report.Cost.Family = ""
	assertContainsAll(t, strings.Join(tokenReportNotes(&mixed), "\n"), "Cost shares mix list-price ratios from more than one model family")
}

func TestContributorLabel(t *testing.T) {
	t.Parallel()

	cases := map[string]tokenreport.Contributor{
		"Skill: artifact-design (loaded)": {Kind: tokenreport.KindSkill, Label: "artifact-design"},
		"Subagent: Explore":               {Kind: tokenreport.KindSubagent, Label: "Explore"},
		"Context replay (cache read)":     {Kind: tokenreport.KindReplay, Label: tokenreport.LabelContextReplay},
		"Prompt & system context":         {Kind: tokenreport.KindPrompt, Label: tokenreport.LabelPromptContext},
		"Bash · during debugging":         {Kind: tokenreport.KindTool, Label: "Bash", Skill: "debugging"},
		"Assistant text · during writing": {Kind: tokenreport.KindText, Label: tokenreport.LabelAssistantText, Skill: "writing"},
		"Read":                            {Kind: tokenreport.KindTool, Label: "Read"},
	}
	for want, c := range cases {
		if got := contributorLabel(&c); got != want {
			t.Errorf("contributorLabel(%+v) = %q, want %q", c, got, want)
		}
	}
}

func TestTokenJSONHelpers(t *testing.T) {
	t.Parallel()

	v := claudeView(wideAttributed(), tokenreport.Recommendation{Kind: tokenreport.RecommendationKindSession, Cause: tokenreport.CauseThinking, Text: "t"})
	usage := tokenUsageJSONFor(&v)
	if usage == nil || usage.Total != 4_154_500 || usage.ThinkingTokens != 65_300 || usage.CacheCreation1hTokens != 332_900 || usage.SubagentTotal != 4_000 {
		t.Errorf("usage JSON = %+v", usage)
	}
	cost := tokenCostJSONFor(&v)
	if cost == nil || cost.Provider != tokenreport.ProviderAnthropic || cost.Weights == nil || cost.Weights.CacheWrite1h != 2 {
		t.Errorf("cost JSON = %+v", cost)
	}
	recs := tokenRecommendationsJSONFor(v.Recommendations)
	if len(recs) != 1 || recs[0].ID != "thinking" || recs[0].Message != "t" {
		t.Errorf("recs JSON = %+v", recs)
	}
	if e := tokenEffortJSONFor(&v); e == nil || e.Value != checkpointTokensFixtureEffort || e.Calls != 43 {
		t.Errorf("effort JSON = %+v", e)
	}
	if tokenDurationSeconds(-time.Second) != 0 || tokenDurationSeconds(90*time.Second) != 90 {
		t.Error("tokenDurationSeconds")
	}
}
