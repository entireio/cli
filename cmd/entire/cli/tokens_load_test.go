package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// testSkillSystematicDebugging is the skill the Claude fixture's first call runs under.
const testSkillSystematicDebugging = "systematic-debugging"

func TestApplySkillEventAnchorsLabelsUnnamedSkillLoads(t *testing.T) {
	t.Parallel()

	attribution := &types.Attribution{Calls: []types.CallUsage{
		{Emitted: []types.ToolUseRef{{ID: "toolu_x", Tool: "Skill"}, {ID: "toolu_named", Tool: "Skill", SkillName: "keep", Detail: "keep"}}},
		{Consumed: []types.ToolResultRef{{ToolUse: types.ToolUseRef{ID: "toolu_x", Tool: "Skill"}}}},
	}}
	applySkillEventAnchors(attribution, []types.SkillEvent{
		{Skill: types.SkillEventSkill{Name: testSkillSystematicDebugging}, TranscriptAnchor: &types.SkillEventTranscriptAnchor{ToolUseID: "toolu_x"}},
		{Skill: types.SkillEventSkill{Name: "other"}, TranscriptAnchor: &types.SkillEventTranscriptAnchor{ToolUseID: "toolu_named"}},
	})
	got := attribution.Calls[0].Emitted[0]
	if got.SkillName != testSkillSystematicDebugging || got.Detail != testSkillSystematicDebugging {
		t.Errorf("emitted ref = %+v, want the anchor's skill name", got)
	}
	if attribution.Calls[0].Emitted[1].SkillName != "keep" {
		t.Errorf("a ref the attributor named must keep its name, got %+v", attribution.Calls[0].Emitted[1])
	}
	if attribution.Calls[1].Consumed[0].ToolUse.SkillName != testSkillSystematicDebugging {
		t.Errorf("consumed ref = %+v", attribution.Calls[1].Consumed[0].ToolUse)
	}
}

func TestModalKeyCountPrefersHighestThenLexical(t *testing.T) {
	t.Parallel()

	k, n := modalKeyCount(map[string]int{"gpt-5.4": 1, "claude-fable-5": 3, "claude-haiku-4-5": 3})
	if k != "claude-fable-5" || n != 3 {
		t.Errorf("got %q/%d, want claude-fable-5/3 (ties resolve lexically)", k, n)
	}
	if k, n := modalKeyCount(nil); k != "" || n != 0 {
		t.Errorf("empty map → %q/%d", k, n)
	}
}

// TestFinishSessionTokenAnalysis_KnownTTLPricesFiveMinuteWrites pins the
// per-call pricing rule: an agent-written usage block with cache writes and
// a zero 1h split is all-5m, so the class shares agree with the contributor
// table's units and the usage table prices the row.
func TestFinishSessionTokenAnalysis_KnownTTLPricesFiveMinuteWrites(t *testing.T) {
	t.Parallel()

	meta := &checkpoint.Metadata{SessionID: "cw-only", Agent: agent.AgentTypeClaudeCode, Model: checkpointTokensFixtureModel}
	attribution := &types.Attribution{Calls: []types.CallUsage{
		{Model: checkpointTokensFixtureModel, Usage: types.TokenUsage{CacheCreationTokens: 4000, APICallCount: 1}},
		{Model: checkpointTokensFixtureModel, Usage: types.TokenUsage{CacheCreationTokens: 6000, APICallCount: 1}},
	}}
	a := sessionTokenAnalysis{meta: meta, attribution: attribution, efforts: map[string]int{}, models: map[string]int{}}
	finishSessionTokenAnalysis(&a)

	summed := tokenreport.SumCostShares(a.costParts...)
	if summed.Units == 0 || summed.Units != a.attributed.PricedUnits || summed.CacheWriteUnpriced {
		t.Fatalf("class units %v (unpriced=%v) must equal contributor units %v", summed.Units, summed.CacheWriteUnpriced, a.attributed.PricedUnits)
	}

	view := assembleTokenReportView([]sessionTokenAnalysis{a}, []*checkpoint.Metadata{meta})
	out := renderBody(&view)
	assertContainsAll(t, out, "Cache write, 5-minute", "(1.25×)", "10k")
	if strings.Contains(out, "TTL not recorded") || strings.Contains(out, tokenTableUnpriced) {
		t.Errorf("all-5m writes from a per-call block are priced, got:\n%s", out)
	}
}

func TestCountUnmatchedSubagentRefsIncludesConsumedOnlyRefs(t *testing.T) {
	t.Parallel()

	spawned := types.ToolUseRef{ID: "toolu_before", Tool: "Agent", SubagentType: "Explore"}
	emitted := types.ToolUseRef{ID: "toolu_in", Tool: "Agent", SubagentType: "Explore"}
	attribution := &types.Attribution{
		Calls: []types.CallUsage{
			{Emitted: []types.ToolUseRef{emitted}, Consumed: []types.ToolResultRef{{ToolUse: spawned}}},
			{Consumed: []types.ToolResultRef{{ToolUse: emitted}, {ToolUse: types.ToolUseRef{ID: "toolu_bash", Tool: "Bash"}}}},
		},
	}
	if got := countUnmatchedSubagentRefs(attribution); got != 2 {
		t.Errorf("unmatched = %d, want 2 (one emitted, one consumed from before the window, each once)", got)
	}
	attribution.Subagents = []types.SubagentRecord{{ToolUseID: "toolu_before", SubagentType: "Explore"}}
	if got := countUnmatchedSubagentRefs(attribution); got != 1 {
		t.Errorf("unmatched after recording the consumed-only ref = %d, want 1", got)
	}
}
