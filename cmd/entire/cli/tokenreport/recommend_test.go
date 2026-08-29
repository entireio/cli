package tokenreport

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Fixture vocabulary. Names that appear in sentences are set in backticks by
// the implementation (model, command, skill, effort, setting) or are cited
// row labels (tool names, subagent types), so the visibility test can tell a
// name's digits ("claude-fable-5") from a figure.
const (
	testModel      = "claude-fable-5"
	testOtherModel = "claude-haiku-4-5"
	testTool       = "Bash"
	testCommand    = "go test ./cmd/entire/..."
	testSkill      = "artifact-design"
	testSubagent   = "Explore"
	testDigitLabel = "gpt5-reviewer"
	testEffort     = "high"
	testLever      = "model_reasoning_effort"
	testUnits      = 1000.0
	unknownAgent   = types.AgentType("Some New Agent")

	phraseOfCost = "of cost"
	phraseMostly = "mostly"
)

// shares builds a priced CostShares for provider p.
func shares(p Provider, input, write, read, output, thinking float64) CostShares {
	return CostShares{Provider: p, Input: input, CacheWrite: write, CacheRead: read, Output: output, Thinking: thinking, Units: testUnits}
}

func detail(name string, calls, tokens int, share float64) Detail {
	return Detail{Detail: name, Calls: calls, Tokens: tokens, CostShare: share}
}

// toolRow is a Bash row whose tokens are fresh input and cache writes (tool
// output read back into context).
func toolRow(share float64, details ...Detail) Contributor {
	return Contributor{
		Kind:      KindTool,
		Label:     testTool,
		Usage:     types.TokenUsage{InputTokens: 4_600, CacheCreationTokens: 160_000, APICallCount: 9},
		CostShare: share,
		Source:    SourceTranscript,
		Details:   details,
	}
}

func replayRow(share float64) Contributor {
	return Contributor{Kind: KindReplay, Label: LabelContextReplay, Usage: types.TokenUsage{CacheReadTokens: 3_700_000}, CostShare: share, Source: SourceTranscript}
}

func textRow(share float64) Contributor {
	return Contributor{Kind: KindText, Label: LabelAssistantText, Usage: types.TokenUsage{OutputTokens: 100_000, APICallCount: 30}, CostShare: share, Source: SourceTranscript}
}

// subagentRow is an Explore subagent row that ran `runs` times on model.
func subagentRow(model string, share float64, runs int) Contributor {
	return labelledSubagentRow(testSubagent, model, share, runs)
}

func labelledSubagentRow(label, model string, share float64, runs int) Contributor {
	return Contributor{
		Kind:      KindSubagent,
		Label:     label,
		Model:     model,
		Usage:     types.TokenUsage{InputTokens: 50_000, CacheReadTokens: 4_500_000, OutputTokens: 150_000, APICallCount: 60},
		CostShare: share,
		Source:    SourceTranscript,
		Details:   []Detail{detail(label, runs, 4_700_000, share)},
	}
}

// skillRow is a skill's own load row, loaded `loads` times.
func skillRow(share float64, loads int) Contributor {
	return Contributor{
		Kind:      KindSkill,
		Label:     testSkill,
		Usage:     types.TokenUsage{InputTokens: 200, CacheCreationTokens: 41_000, OutputTokens: 100, APICallCount: loads},
		CostShare: share,
		Source:    SourceTranscript,
		Details:   []Detail{detail(testSkill, loads, 41_300, share)},
	}
}

// quietReport is a priced Claude Code report that trips no gate: 2h long,
// cache read 50% of cost (write 30%, not the largest), no subagent or skill
// rows, thinking 17% of output, Anthropic-priced.
func quietReport() Report {
	return Report{
		Agent:   agentClaudeCode,
		Profile: ProfileFor(agentClaudeCode),
		Model:   testModel,
		Effort:  testEffort,
		Usage: types.TokenUsage{
			InputTokens: 6_500, CacheCreationTokens: 332_900, CacheReadTokens: 3_700_000,
			OutputTokens: 115_100, ThinkingTokens: 20_000, APICallCount: 43,
		},
		Cost: shares(ProviderAnthropic, 0.02, 0.30, 0.50, 0.18, 0.03),
		Attributed: Attributed{
			PricedUnits:  testUnits,
			Contributors: []Contributor{replayRow(0.50), toolRow(0.20, detail(testCommand, 9, 140_200, 0.17)), textRow(0.18)},
		},
		Duration: 2 * time.Hour,
		Calls:    43,
	}
}

// codexReport is a priced Codex report that trips no gate: fresh input 30%
// (below the 45% cache_miss gate), cache read 50%, thinking 20% of output.
func codexReport() Report {
	r := quietReport()
	r.Agent = agentCodex
	r.Profile = ProfileFor(agentCodex)
	r.Model = "gpt-5.4"
	r.Usage.CacheCreationTokens = 0
	r.Usage.ThinkingTokens = 23_000
	r.Cost = shares(ProviderOpenAI, 0.30, 0, 0.50, 0.20, 0.04)
	return r
}

// Firing fixtures, shared by the per-cause tests and the visibility test.

func longSessionByDuration() Report {
	r := quietReport()
	r.Duration = GatesFor(agentClaudeCode).LongSessionDuration
	return r
}

func longSessionByReplay() Report {
	r := quietReport()
	g := GatesFor(agentClaudeCode)
	r.Cost = shares(ProviderAnthropic, 0.02, 0.15, g.LongSessionCacheReadShare, 0.13, 0.03)
	r.Attributed.Contributors[0].CostShare = g.LongSessionCacheReadShare
	r.Calls = g.LongSessionMinCalls
	r.Usage.APICallCount = g.LongSessionMinCalls
	return r
}

func contextGrowthReport(withDetail bool) Report {
	r := quietReport()
	g := GatesFor(agentClaudeCode)
	r.Cost = shares(ProviderAnthropic, 0.05, 0.45, 0.35, 0.15, 0.03)
	var details []Detail
	if withDetail {
		details = []Detail{detail(testCommand, 9, 140_200, 0.22)}
	}
	r.Attributed.Contributors = []Contributor{replayRow(0.35), toolRow(g.ContextGrowthRowShare, details...), textRow(0.15)}
	return r
}

func subagentModelReport(model string, share float64) Report {
	r := quietReport()
	r.Cost = shares(ProviderAnthropic, 0.02, 0.30, 0.45, 0.23, 0.03)
	r.Attributed.Contributors = []Contributor{replayRow(0.45), subagentRow(model, share, 5), toolRow(0.20), textRow(0.15)}
	return r
}

func thinkingReport(verified bool) Report {
	r := codexReport()
	g := GatesFor(agentCodex)
	r.Usage.OutputTokens = 100_000
	r.Usage.ThinkingTokens = int(float64(r.Usage.OutputTokens) * g.ThinkingShare)
	r.Cost.Thinking = 0.10
	r.Profile.EffortSettingVerified = verified
	r.Profile.Levers = []string{testLever}
	return r
}

func cacheMissReport(agent types.AgentType, p Provider, inputShare float64) Report {
	r := codexReport()
	r.Agent = agent
	r.Profile = ProfileFor(agent)
	r.Cost = shares(p, inputShare, 0, 0.15, 1-inputShare-0.15, 0.03)
	r.Attributed.Contributors = []Contributor{toolRow(0.31, detail(testCommand, 12, 1_200_000, 0.31)), replayRow(0.15), textRow(0.13)}
	return r
}

func repeatedSkillReport(loads int) Report {
	r := quietReport()
	r.Attributed.Contributors = append(r.Attributed.Contributors, skillRow(0.04, loads))
	return r
}

// deepSkillReport ranks the skill row 9th (index 8): replay, seven bigger
// tool rows, then the skill loaded 3 times at a share below 0.5%. Only
// repeated_skill fires, and its figures are visible only because the row is
// cited.
func deepSkillReport() Report {
	r := quietReport()
	rows := []Contributor{replayRow(0.50)}
	for i := range 7 {
		row := toolRow(0.07)
		row.Label = "Tool" + string(rune('A'+i))
		rows = append(rows, row)
	}
	rows = append(rows, skillRow(0.003, 3))
	r.Attributed.Contributors = rows
	return r
}

// capReport fires four causes: context_growth (row 40%), long_session (cache
// read 30%, 9h), subagent_model (20%), repeated_skill (5%).
func capReport() Report {
	r := quietReport()
	r.Duration = 9 * time.Hour
	r.Cost = shares(ProviderAnthropic, 0.05, 0.45, 0.30, 0.20, 0.03)
	r.Attributed.Contributors = []Contributor{
		toolRow(0.40, detail(testCommand, 9, 140_200, 0.35)), replayRow(0.30), subagentRow(testModel, 0.20, 3), textRow(0.05), skillRow(0.05, 3),
	}
	return r
}

// volumeOnlyReport has no priced units: a 9h Claude session with a subagent
// row and a skill loaded 3 times, thinking 60% of output.
func volumeOnlyReport() Report {
	r := quietReport()
	r.Duration = 9 * time.Hour
	r.Cost = CostShares{}
	r.Usage.OutputTokens = 100_000
	r.Usage.ThinkingTokens = 60_000
	r.Attributed = Attributed{Contributors: []Contributor{replayRow(0), toolRow(0), subagentRow(testModel, 0, 5), skillRow(0, 3)}}
	return r
}

// Assertion helpers.

func causes(recs []Recommendation) []Cause {
	out := make([]Cause, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.Cause)
	}
	return out
}

func mustFireOnly(t *testing.T, recs []Recommendation, want Cause) Recommendation {
	t.Helper()
	if len(recs) != 1 || recs[0].Cause != want {
		t.Fatalf("got causes %v, want exactly [%s]", causes(recs), want)
	}
	rec := recs[0]
	if rec.Kind != RecommendationKindSession || rec.Memory != "" || rec.Seen != 0 || rec.Of != 0 {
		t.Fatalf("session recommendation carries pattern fields: %+v", rec)
	}
	return rec
}

func mustFireNothing(t *testing.T, recs []Recommendation) {
	t.Helper()
	if len(recs) != 0 {
		t.Fatalf("expected no recommendation, got %v: %q", causes(recs), recs[0].Text)
	}
}

func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("text %q does not contain %q", text, want)
	}
}

func mustNotContain(t *testing.T, text, unwanted string) {
	t.Helper()
	if strings.Contains(text, unwanted) {
		t.Fatalf("text %q must not contain %q", text, unwanted)
	}
}

func mustCite(t *testing.T, rec Recommendation, want ...Citation) {
	t.Helper()
	if !slices.Equal(rec.Cited, want) {
		t.Fatalf("%s cited %+v, want %+v", rec.Cause, rec.Cited, want)
	}
}

func TestRecommend_QuietReportFiresNothing(t *testing.T) {
	t.Parallel()
	mustFireNothing(t, Recommend(quietReport()))
	mustFireNothing(t, Recommend(codexReport()))
}

func TestRecommend_LongSession(t *testing.T) {
	t.Parallel()

	t.Run("claude duration at 8h fires", func(t *testing.T) {
		t.Parallel()
		r := longSessionByDuration()
		rec := mustFireOnly(t, Recommend(r), CauseLongSession)
		mustContain(t, rec.Text, FormatDuration(r.Duration))
		mustContain(t, rec.Text, FormatTokenCount(r.Usage.CacheReadTokens))
		mustContain(t, rec.Text, FormatPercent(r.Cost.CacheRead)+" "+phraseOfCost)
		mustCite(t, rec)
	})
	t.Run("claude duration one minute short misses", func(t *testing.T) {
		t.Parallel()
		r := longSessionByDuration()
		r.Duration -= time.Minute
		mustFireNothing(t, Recommend(r))
	})
	t.Run("other agent fires at 4h", func(t *testing.T) {
		t.Parallel()
		r := codexReport()
		g := GatesFor(agentCodex)
		if g.LongSessionDuration != 4*time.Hour {
			t.Fatalf("codex long-session gate = %s, want 4h", g.LongSessionDuration)
		}
		r.Duration = g.LongSessionDuration
		mustFireOnly(t, Recommend(r), CauseLongSession)
		r.Duration -= time.Minute
		mustFireNothing(t, Recommend(r))
	})
	t.Run("cache-read arm fires at 70% and 20 calls", func(t *testing.T) {
		t.Parallel()
		r := longSessionByReplay()
		rec := mustFireOnly(t, Recommend(r), CauseLongSession)
		mustContain(t, rec.Text, FormatPercent(r.Cost.CacheRead))
		mustContain(t, rec.Text, strconv.Itoa(r.Calls)+" calls")
		mustNotContain(t, rec.Text, FormatDuration(r.Duration))
	})
	t.Run("cache-read arm misses at 69%", func(t *testing.T) {
		t.Parallel()
		r := longSessionByReplay()
		r.Cost.CacheRead -= 0.01
		mustFireNothing(t, Recommend(r))
	})
	t.Run("cache-read arm misses at 19 calls", func(t *testing.T) {
		t.Parallel()
		r := longSessionByReplay()
		r.Calls--
		r.Usage.APICallCount = r.Calls
		mustFireNothing(t, Recommend(r))
	})
	t.Run("calls fall back to APICallCount", func(t *testing.T) {
		t.Parallel()
		r := longSessionByReplay()
		r.Calls = 0
		mustFireOnly(t, Recommend(r), CauseLongSession)
	})
}

func TestRecommend_ContextGrowth(t *testing.T) {
	t.Parallel()

	t.Run("cache write largest and row at 25% fires naming and citing the detail", func(t *testing.T) {
		t.Parallel()
		r := contextGrowthReport(true)
		rec := mustFireOnly(t, Recommend(r), CauseContextGrowth)
		row := r.Attributed.Contributors[1]
		mustContain(t, rec.Text, FormatPercent(row.CostShare)+" of the cost was "+testTool+" output")
		mustContain(t, rec.Text, phraseMostly+" `"+testCommand+"` (9 calls, "+FormatTokenCount(row.Details[0].Tokens)+" tokens, "+FormatPercent(row.Details[0].CostShare)+")")
		mustCite(t, rec, Citation{Kind: KindTool, Label: testTool, Detail: testCommand})
	})
	t.Run("row without details stops at the tool and cites only the row", func(t *testing.T) {
		t.Parallel()
		rec := mustFireOnly(t, Recommend(contextGrowthReport(false)), CauseContextGrowth)
		mustNotContain(t, rec.Text, phraseMostly)
		mustCite(t, rec, Citation{Kind: KindTool, Label: testTool})
	})
	t.Run("row at 24% misses", func(t *testing.T) {
		t.Parallel()
		r := contextGrowthReport(true)
		r.Attributed.Contributors[1].CostShare -= 0.01
		mustFireNothing(t, Recommend(r))
	})
	t.Run("row at 30% but cache read largest misses", func(t *testing.T) {
		t.Parallel()
		r := contextGrowthReport(true)
		r.Attributed.Contributors[1].CostShare = 0.30
		r.Cost = shares(ProviderAnthropic, 0.05, 0.35, 0.45, 0.15, 0.03)
		mustFireNothing(t, Recommend(r))
	})
	t.Run("a tie for largest class does not count", func(t *testing.T) {
		t.Parallel()
		r := contextGrowthReport(true)
		r.Cost = shares(ProviderAnthropic, 0.05, 0.40, 0.40, 0.15, 0.03)
		mustFireNothing(t, Recommend(r))
	})
	t.Run("replay row does not qualify", func(t *testing.T) {
		t.Parallel()
		r := contextGrowthReport(true)
		r.Attributed.Contributors[1].CostShare = 0.10
		// Replay row keeps 35%: above the row gate, but it carries no cache writes.
		mustFireNothing(t, Recommend(r))
	})
	t.Run("skill annotation on the tool row is named and cited", func(t *testing.T) {
		t.Parallel()
		r := contextGrowthReport(false)
		r.Attributed.Contributors[1].Skill = "systematic-debugging"
		rec := mustFireOnly(t, Recommend(r), CauseContextGrowth)
		mustContain(t, rec.Text, testTool+" output (during systematic-debugging)")
		mustCite(t, rec, Citation{Kind: KindTool, Label: testTool, Skill: "systematic-debugging"})
	})
}

func TestRecommend_SubagentModel(t *testing.T) {
	t.Parallel()
	g := GatesFor(agentClaudeCode)

	t.Run("row at 15% on the parent model fires citing the run-count detail", func(t *testing.T) {
		t.Parallel()
		r := subagentModelReport(testModel, g.SubagentModelShare)
		rec := mustFireOnly(t, Recommend(r), CauseSubagentModel)
		mustContain(t, rec.Text, testSubagent+" subagents ran 5 times on `"+testModel+"`")
		mustContain(t, rec.Text, FormatPercent(g.SubagentModelShare)+" "+phraseOfCost)
		mustCite(t, rec, Citation{Kind: KindSubagent, Label: testSubagent, Detail: testSubagent})
	})
	t.Run("row at 14% misses", func(t *testing.T) {
		t.Parallel()
		mustFireNothing(t, Recommend(subagentModelReport(testModel, g.SubagentModelShare-0.01)))
	})
	t.Run("row on a different model misses", func(t *testing.T) {
		t.Parallel()
		mustFireNothing(t, Recommend(subagentModelReport(testOtherModel, 0.30)))
	})
	t.Run("row with empty model fires and names the session model", func(t *testing.T) {
		t.Parallel()
		rec := mustFireOnly(t, Recommend(subagentModelReport("", g.SubagentModelShare)), CauseSubagentModel)
		mustContain(t, rec.Text, "on `"+testModel+"`")
	})
	t.Run("both models empty names the session's own model", func(t *testing.T) {
		t.Parallel()
		r := subagentModelReport("", g.SubagentModelShare)
		r.Model = ""
		rec := mustFireOnly(t, Recommend(r), CauseSubagentModel)
		mustContain(t, rec.Text, "ran 5 times on the session's own model")
	})
	t.Run("unknown run count is omitted and only the row is cited", func(t *testing.T) {
		t.Parallel()
		r := subagentModelReport(testModel, 0.30)
		r.Attributed.Contributors[1].Details = nil
		rec := mustFireOnly(t, Recommend(r), CauseSubagentModel)
		mustContain(t, rec.Text, testSubagent+" subagents ran on `"+testModel+"`")
		mustNotContain(t, rec.Text, " times")
		mustCite(t, rec, Citation{Kind: KindSubagent, Label: testSubagent})
	})
}

func TestRecommend_Thinking(t *testing.T) {
	t.Parallel()

	t.Run("50% of output with effort recorded fires naming the effort value", func(t *testing.T) {
		t.Parallel()
		r := thinkingReport(false)
		rec := mustFireOnly(t, Recommend(r), CauseThinking)
		mustContain(t, rec.Text, "Thinking took 50% of output tokens ("+FormatTokenCount(r.Usage.ThinkingTokens)+" of "+FormatTokenCount(r.Usage.OutputTokens)+", "+FormatPercent(r.Cost.Thinking)+" "+phraseOfCost+")")
		mustContain(t, rec.Text, "at effort `"+testEffort+"`")
		mustCite(t, rec)
	})
	t.Run("49% misses", func(t *testing.T) {
		t.Parallel()
		r := thinkingReport(false)
		r.Usage.ThinkingTokens--
		mustFireNothing(t, Recommend(r))
	})
	t.Run("agent without effort recording misses", func(t *testing.T) {
		t.Parallel()
		r := thinkingReport(false)
		r.Profile.RecordsEffort = false
		mustFireNothing(t, Recommend(r))
	})
	t.Run("no setting name unless verified", func(t *testing.T) {
		t.Parallel()
		rec := mustFireOnly(t, Recommend(thinkingReport(false)), CauseThinking)
		mustNotContain(t, rec.Text, testLever)
		mustContain(t, rec.Text, "a lower effort setting")
	})
	t.Run("verified setting name is printed", func(t *testing.T) {
		t.Parallel()
		rec := mustFireOnly(t, Recommend(thinkingReport(true)), CauseThinking)
		mustContain(t, rec.Text, "lowering `"+testLever+"`")
	})
	t.Run("verified without a lever stops at the cause", func(t *testing.T) {
		t.Parallel()
		r := thinkingReport(true)
		r.Profile.Levers = nil
		rec := mustFireOnly(t, Recommend(r), CauseThinking)
		mustNotContain(t, rec.Text, testLever)
	})
	t.Run("unknown effort value is omitted", func(t *testing.T) {
		t.Parallel()
		r := thinkingReport(false)
		r.Effort = ""
		rec := mustFireOnly(t, Recommend(r), CauseThinking)
		mustNotContain(t, rec.Text, "at effort")
	})
}

func TestRecommend_CacheMiss(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		agent types.AgentType
		p     Provider
	}{
		{name: "codex at 45%", agent: agentCodex, p: ProviderOpenAI},
		{name: "opencode at 40%", agent: agentOpenCode, p: ProviderOpenAI},
		{name: "gemini at 70%", agent: agentGemini, p: ProviderGoogle},
	}
	for _, tc := range cases {
		t.Run(tc.name+" fires", func(t *testing.T) {
			t.Parallel()
			g := GatesFor(tc.agent)
			r := cacheMissReport(tc.agent, tc.p, g.CacheMissShare)
			rec := mustFireOnly(t, Recommend(r), CauseCacheMiss)
			mustContain(t, rec.Text, FormatPercent(g.CacheMissShare)+" of the cost was uncached input")
			mustContain(t, rec.Text, phraseMostly+" "+testTool+" results, led by `"+testCommand+"` (12 calls, 1.2M tokens, 31%)")
			mustCite(t, rec, Citation{Kind: KindTool, Label: testTool, Detail: testCommand})
		})
		t.Run(tc.name+" one point short misses", func(t *testing.T) {
			t.Parallel()
			g := GatesFor(tc.agent)
			mustFireNothing(t, Recommend(cacheMissReport(tc.agent, tc.p, g.CacheMissShare-0.01)))
		})
	}
	t.Run("claude code never fires", func(t *testing.T) {
		t.Parallel()
		mustFireNothing(t, Recommend(cacheMissReport(agentClaudeCode, ProviderAnthropic, 0.60)))
	})
	t.Run("anthropic-priced pi never fires", func(t *testing.T) {
		t.Parallel()
		mustFireNothing(t, Recommend(cacheMissReport(agentPi, ProviderAnthropic, 0.60)))
	})
	t.Run("openai-priced pi fires at the default gate", func(t *testing.T) {
		t.Parallel()
		mustFireOnly(t, Recommend(cacheMissReport(agentPi, ProviderOpenAI, GatesFor(agentPi).CacheMissShare)), CauseCacheMiss)
	})
	t.Run("mixed provider never fires", func(t *testing.T) {
		t.Parallel()
		mustFireNothing(t, Recommend(cacheMissReport(agentCodex, "", 0.60)))
	})
	t.Run("without a tool row the sentence stops at the cause and cites nothing", func(t *testing.T) {
		t.Parallel()
		r := cacheMissReport(agentCodex, ProviderOpenAI, 0.50)
		r.Attributed.Contributors = []Contributor{replayRow(0.15), textRow(0.13)}
		rec := mustFireOnly(t, Recommend(r), CauseCacheMiss)
		mustNotContain(t, rec.Text, phraseMostly)
		mustContain(t, rec.Text, "instead of from the cache.")
		mustCite(t, rec)
	})
	t.Run("tool row without details quotes and cites the row", func(t *testing.T) {
		t.Parallel()
		r := cacheMissReport(agentCodex, ProviderOpenAI, 0.50)
		r.Attributed.Contributors[0].Details = nil
		rec := mustFireOnly(t, Recommend(r), CauseCacheMiss)
		mustContain(t, rec.Text, phraseMostly+" "+testTool+" results (164.6k tokens, 31% "+phraseOfCost+")")
		mustCite(t, rec, Citation{Kind: KindTool, Label: testTool})
	})
}

func TestRecommend_RepeatedSkill(t *testing.T) {
	t.Parallel()
	g := GatesFor(agentClaudeCode)

	t.Run("two loads fires citing the load-count detail", func(t *testing.T) {
		t.Parallel()
		r := repeatedSkillReport(g.RepeatedSkillMinLoads)
		rec := mustFireOnly(t, Recommend(r), CauseRepeatedSkill)
		mustContain(t, rec.Text, "`"+testSkill+"` was loaded "+strconv.Itoa(g.RepeatedSkillMinLoads)+" times (41.3k tokens, 4% "+phraseOfCost+")")
		mustCite(t, rec, Citation{Kind: KindSkill, Label: testSkill, Detail: testSkill})
	})
	t.Run("one load misses", func(t *testing.T) {
		t.Parallel()
		mustFireNothing(t, Recommend(repeatedSkillReport(g.RepeatedSkillMinLoads-1)))
	})
	t.Run("loads are counted from the skill's own detail, not APICallCount", func(t *testing.T) {
		t.Parallel()
		r := repeatedSkillReport(1)
		r.Attributed.Contributors[3].Usage.APICallCount = 5
		mustFireNothing(t, Recommend(r))
	})
	t.Run("a skill row without its own detail cannot fire", func(t *testing.T) {
		t.Parallel()
		r := repeatedSkillReport(3)
		r.Attributed.Contributors[3].Details = nil
		mustFireNothing(t, Recommend(r))
	})
	t.Run("a deep-ranked skill row fires with a <1% share", func(t *testing.T) {
		t.Parallel()
		rec := mustFireOnly(t, Recommend(deepSkillReport()), CauseRepeatedSkill)
		mustContain(t, rec.Text, "(41.3k tokens, <1% "+phraseOfCost+")")
		mustCite(t, rec, Citation{Kind: KindSkill, Label: testSkill, Detail: testSkill})
	})
}

func TestRecommend_CapAndOrder(t *testing.T) {
	t.Parallel()
	recs := Recommend(capReport())
	got := causes(recs)
	want := []Cause{CauseContextGrowth, CauseLongSession}
	if !slices.Equal(got, want) {
		t.Fatalf("causes = %v, want %v", got, want)
	}
}

func TestRecommend_VolumeOnly(t *testing.T) {
	t.Parallel()

	t.Run("cost-share gates stay quiet; duration and skill fire without shares", func(t *testing.T) {
		t.Parallel()
		r := volumeOnlyReport()
		r.Usage.ThinkingTokens = 20_000
		recs := Recommend(r)
		got := causes(recs)
		if !slices.Equal(got, []Cause{CauseLongSession, CauseRepeatedSkill}) {
			t.Fatalf("causes = %v, want [long_session repeated_skill]", got)
		}
		for _, rec := range recs {
			mustNotContain(t, rec.Text, "%")
		}
	})
	t.Run("thinking fires on volume without a cost clause", func(t *testing.T) {
		t.Parallel()
		r := volumeOnlyReport()
		r.Duration = 0
		r.Attributed.Contributors = nil
		rec := mustFireOnly(t, Recommend(r), CauseThinking)
		mustContain(t, rec.Text, "60% of output tokens (60k of 100k)")
		mustNotContain(t, rec.Text, phraseOfCost)
	})
}

// Figure extraction for the visibility test.
var (
	durationTokens = regexp.MustCompile(`\d+d \d+h|\d+h \d+m|\d+m\b|\d+s\b`)
	numericTokens  = regexp.MustCompile(`<1%|\d[\d.,]*[kM%]?`)
	backtickSpans  = regexp.MustCompile("`[^`]*`")
)

// extractFigures returns the numeric tokens in text: FormatDuration forms
// first, then FormatPercent's "<1%", FormatTokenCount and plain-integer
// forms. Backticked spans (model, command, skill, effort, setting) and the
// labels and skills of cited rows (tool names, subagent types) are names, not
// figures, and are skipped.
func extractFigures(text string, cited []Citation) []string {
	text = backtickSpans.ReplaceAllString(text, " ")
	for _, c := range cited {
		text = strings.ReplaceAll(text, c.Label, " ")
		if c.Skill != "" {
			text = strings.ReplaceAll(text, c.Skill, " ")
		}
	}
	figures := durationTokens.FindAllString(text, -1)
	text = durationTokens.ReplaceAllString(text, " ")
	for _, tok := range numericTokens.FindAllString(text, -1) {
		figures = append(figures, strings.TrimRight(tok, ".,"))
	}
	return figures
}

// citesRow reports whether cited names row c (by Kind, Label, Skill).
func citesRow(cited []Citation, c *Contributor) bool {
	return slices.ContainsFunc(cited, func(ct Citation) bool {
		return ct.Kind == c.Kind && ct.Label == c.Label && ct.Skill == c.Skill
	})
}

// citesDetail reports whether cited names detail d of row c.
func citesDetail(cited []Citation, c *Contributor, d *Detail) bool {
	return slices.ContainsFunc(cited, func(ct Citation) bool {
		return ct.Kind == c.Kind && ct.Label == c.Label && ct.Skill == c.Skill && ct.Detail == d.Detail
	})
}

// renderedFigures is the set of figures the renderer prints from r given the
// recommendations' citations, in the renderer's own formatting: every usage
// class, every cost class share, the thinking share of output, the
// duration, the call counts; the tokens and share of every row ranked below
// MaxRenderedRows or cited; the calls, tokens and share of every detail
// under a row ranked below MaxRenderedDetails or cited by name.
func renderedFigures(r *Report, cited []Citation) map[string]bool {
	set := map[string]bool{}
	u := &r.Usage
	for _, n := range []int{u.InputTokens, u.CacheCreationTokens, u.CacheReadTokens, u.OutputTokens, u.ThinkingTokens, u.CacheCreation1hTokens} {
		set[FormatTokenCount(n)] = true
	}
	for _, s := range []float64{r.Cost.Input, r.Cost.CacheWrite, r.Cost.CacheRead, r.Cost.Output, r.Cost.Thinking} {
		set[FormatPercent(s)] = true
	}
	if u.OutputTokens > 0 {
		set[FormatPercent(float64(u.ThinkingTokens)/float64(u.OutputTokens))] = true
	}
	set[FormatDuration(r.Duration)] = true
	set[strconv.Itoa(r.Calls)] = true
	set[strconv.Itoa(u.APICallCount)] = true
	for i := range r.Attributed.Contributors {
		c := &r.Attributed.Contributors[i]
		if i < MaxRenderedRows || citesRow(cited, c) {
			set[FormatTokenCount(volume(&c.Usage))] = true
			set[FormatPercent(c.CostShare)] = true
		}
		for j := range c.Details {
			d := &c.Details[j]
			if i < MaxRenderedDetails || citesDetail(cited, c, d) {
				set[strconv.Itoa(d.Calls)] = true
				set[FormatTokenCount(d.Tokens)] = true
				set[FormatPercent(d.CostShare)] = true
			}
		}
	}
	return set
}

// unrenderedFigures returns the figures of recs' Text that renderedFigures
// does not contain.
func unrenderedFigures(r *Report, recs []Recommendation, cited []Citation) []string {
	rendered := renderedFigures(r, cited)
	var missing []string
	for _, rec := range recs {
		for _, f := range extractFigures(rec.Text, rec.Cited) {
			if !rendered[f] {
				missing = append(missing, f)
			}
		}
	}
	return missing
}

func allCited(recs []Recommendation) []Citation {
	var out []Citation
	for _, rec := range recs {
		out = append(out, rec.Cited...)
	}
	return out
}

func TestRecommend_EveryFigureIsRendered(t *testing.T) {
	t.Parallel()

	fixtures := map[string]Report{
		"long_session by duration":      longSessionByDuration(),
		"long_session by replay":        longSessionByReplay(),
		"context_growth with detail":    contextGrowthReport(true),
		"context_growth without detail": contextGrowthReport(false),
		"subagent_model":                subagentModelReport(testModel, 0.21),
		"subagent_model empty model":    subagentModelReport("", 0.21),
		"thinking":                      thinkingReport(true),
		"cache_miss codex":              cacheMissReport(agentCodex, ProviderOpenAI, 0.49),
		"cache_miss gemini":             cacheMissReport(agentGemini, ProviderGoogle, 0.72),
		"repeated_skill":                repeatedSkillReport(3),
		"repeated_skill deep-ranked":    deepSkillReport(),
		"cap and order":                 capReport(),
		"volume only":                   volumeOnlyReport(),
		"cache_miss tool row no details": func() Report {
			r := cacheMissReport(agentCodex, ProviderOpenAI, 0.50)
			r.Attributed.Contributors[0].Details = nil
			return r
		}(),
		"cache_miss without tool row": func() Report {
			r := cacheMissReport(agentCodex, ProviderOpenAI, 0.50)
			r.Attributed.Contributors = nil
			return r
		}(),
		"subagent_model without run count": func() Report {
			r := subagentModelReport(testModel, 0.30)
			r.Attributed.Contributors[1].Details = nil
			return r
		}(),
		"subagent_model label with a digit": func() Report {
			r := subagentModelReport(testModel, 0.30)
			r.Attributed.Contributors[1] = labelledSubagentRow(testDigitLabel, testModel, 0.30, 5)
			return r
		}(),
	}
	for name, r := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			recs := Recommend(r)
			if len(recs) == 0 {
				t.Fatal("fixture fired nothing")
			}
			for _, rec := range recs {
				if len(extractFigures(rec.Text, rec.Cited)) == 0 {
					t.Fatalf("%s: no figures found in %q", rec.Cause, rec.Text)
				}
			}
			if missing := unrenderedFigures(&r, recs, allCited(recs)); len(missing) > 0 {
				t.Errorf("recommendations quote %v, which no rendered row carries: %+v", missing, recs)
			}
		})
	}
}

// TestRecommend_DeepRowVisibleOnlyByCitation pins the contract: a skill row
// ranked below MaxRenderedRows is visible only because the recommendation
// cites it — without the citation its figures are not rendered.
func TestRecommend_DeepRowVisibleOnlyByCitation(t *testing.T) {
	t.Parallel()
	r := deepSkillReport()
	recs := Recommend(r)
	rec := mustFireOnly(t, recs, CauseRepeatedSkill)
	if idx := slices.IndexFunc(r.Attributed.Contributors, func(c Contributor) bool { return c.Kind == KindSkill }); idx < MaxRenderedRows {
		t.Fatalf("fixture ranks the skill row at %d, want ≥ %d", idx, MaxRenderedRows)
	}
	mustCite(t, rec, Citation{Kind: KindSkill, Label: testSkill, Detail: testSkill})
	if missing := unrenderedFigures(&r, recs, rec.Cited); len(missing) > 0 {
		t.Fatalf("with the citation, %v are unrendered", missing)
	}
	missing := unrenderedFigures(&r, recs, nil)
	want := []string{"3", "41.3k", "<1%"}
	if !slices.Equal(missing, want) {
		t.Fatalf("without the citation, unrendered = %v, want %v", missing, want)
	}
}

func TestExtractFigures(t *testing.T) {
	t.Parallel()
	cited := []Citation{{Kind: KindSubagent, Label: testDigitLabel}}
	got := extractFigures("This session ran 9h 42m over 43 calls; gpt5-reviewer subagents on `claude-fable-5` read 3.7M tokens (23% of cost, <1% here), 1d 2h, 6m, 42s, 5.", cited)
	want := []string{"9h 42m", "1d 2h", "6m", "42s", "43", "3.7M", "23%", "<1%", "5"}
	if !slices.Equal(got, want) {
		t.Fatalf("extractFigures = %v, want %v", got, want)
	}
}

func TestGatesFor(t *testing.T) {
	t.Parallel()

	common := Gates{
		LongSessionDuration:       4 * time.Hour,
		LongSessionCacheReadShare: 0.70,
		LongSessionMinCalls:       20,
		ContextGrowthRowShare:     0.25,
		SubagentModelShare:        0.15,
		ThinkingShare:             0.50,
		CacheMissShare:            0.45,
		RepeatedSkillMinLoads:     2,
	}
	with := func(f func(*Gates)) Gates { g := common; f(&g); return g }

	cases := []struct {
		agent types.AgentType
		want  Gates
	}{
		{agent: agentClaudeCode, want: with(func(g *Gates) { g.LongSessionDuration = 8 * time.Hour })},
		{agent: agentCodex, want: common},
		{agent: agentOpenCode, want: with(func(g *Gates) { g.CacheMissShare = 0.40 })},
		{agent: agentGemini, want: with(func(g *Gates) { g.CacheMissShare = 0.70 })},
		{agent: agentPi, want: common},
		{agent: agentCursor, want: common},
		{agent: agentCopilotCLI, want: common},
		{agent: agentFactoryAIDroid, want: common},
		{agent: unknownAgent, want: common},
	}
	for _, tc := range cases {
		t.Run(string(tc.agent), func(t *testing.T) {
			t.Parallel()
			if got := GatesFor(tc.agent); got != tc.want {
				t.Fatalf("GatesFor(%q) = %+v, want %+v", tc.agent, got, tc.want)
			}
		})
	}
}
