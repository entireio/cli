package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// piShapedView is a Pi-shaped report: effort recorded, no thinking, an
// agent-reported cost, and a priced OpenAI usage.
func piShapedView() tokenReportView {
	usage := types.TokenUsage{InputTokens: 40_000, CacheReadTokens: 200_000, OutputTokens: 6_000, APICallCount: 12}
	w, _, _ := tokenreport.WeightsFor("gpt-5.4")
	return tokenReportView{
		Report: tokenreport.Report{
			Agent: agent.AgentTypePi, Profile: tokenreport.ProfileFor(agent.AgentTypePi), Model: "gpt-5.4", Effort: "medium",
			Usage: usage, Cost: tokenreport.ComputeCostShares(&usage, w), Duration: 42 * time.Minute, Calls: 12,
			Attributed: tokenreport.Attributed{Contributors: []tokenreport.Contributor{}},
		},
		HasUsage: true, EffortCalls: 12, Attributed: true, AgentReportedCost: 1.2345,
	}
}

func TestWriteCheckpointTokensText_PiShapedPrintsAgentReportedCostAndEffort(t *testing.T) {
	t.Parallel()

	report := checkpointTokensReport{CheckpointID: "01M0ZSZHQ8F6", SessionCount: 1, SessionID: "pi-session", Agent: "Pi", Agents: []string{"Pi"}, Model: "gpt-5.4", Models: []string{"gpt-5.4"}}
	report.applyView(piShapedView())

	var b strings.Builder
	writeCheckpointTokensText(&b, &report)
	out := b.String()
	assertContainsAll(t, out,
		"Checkpoint: 01M0ZSZHQ8F6      Agent: Pi      Model: gpt-5.4",
		"Duration:   42m · 12 API calls · 246k tokens      Effort: medium (12 calls)",
		"Agent-reported cost $1.23",
		"of which thinking",
		"not recorded",
	)
	if report.AgentReportedCost != 1.2345 {
		t.Errorf("agent_reported_cost = %v", report.AgentReportedCost)
	}
}

func TestWriteCheckpointTokensHeader_OmitsEffortWhenProfileDoesNotRecordIt(t *testing.T) {
	t.Parallel()

	v := piShapedView()
	v.Report.Agent = agent.AgentTypeOpenCode
	v.Report.Profile = tokenreport.ProfileFor(agent.AgentTypeOpenCode)
	report := checkpointTokensReport{CheckpointID: "abc", SessionCount: 1, Agents: []string{"OpenCode"}}
	report.applyView(v)

	var b strings.Builder
	writeCheckpointTokensHeader(&b, &report)
	if strings.Contains(b.String(), "Effort:") {
		t.Errorf("OpenCode records no effort, got:\n%s", b.String())
	}
	if report.Effort != nil {
		t.Errorf("effort JSON = %+v, want nil", report.Effort)
	}
}

func TestWriteCheckpointTokensHeader_MultiSessionAndLegacy(t *testing.T) {
	t.Parallel()

	v := piShapedView()
	v.Legacy = &tokenLegacyInfo{Cumulative: true}
	report := checkpointTokensReport{CheckpointID: "abc", SessionCount: 2, Agents: []string{"Claude Code", "Gemini CLI"}, Models: []string{"a", "b"}, Branch: "main"}
	report.applyView(v)

	var b strings.Builder
	writeCheckpointTokensHeader(&b, &report)
	assertContainsAll(t, b.String(),
		"Checkpoint: abc      Agents: Claude Code, Gemini CLI      Models: a, b",
		"Sessions:   2",
		"Branch:     main",
		checkpointTokensLegacyScope,
	)
}

func TestWriteCheckpointTokenComparison_PrintsCostShareLines(t *testing.T) {
	t.Parallel()

	comparison := &checkpointTokensComparison{
		BaselineCheckpointID: "base", TargetCheckpointID: "cur", Status: checkpointComparisonStatusObservedNoChange,
		Total:    buildCheckpointMetricDelta(200, 200),
		Input:    buildCheckpointMetricDelta(100, 100),
		Output:   buildCheckpointMetricDelta(100, 100),
		APICalls: buildCheckpointMetricDelta(0, 3),
		CostShare: buildCheckpointCostShareDelta(
			&tokenreport.CostShares{Input: 0.5, Output: 0.5},
			&tokenreport.CostShares{Input: 0.3, Output: 0.7},
		),
		Qualification: checkpointComparisonQualification(checkpointComparisonStatusObservedNoChange),
	}
	var b strings.Builder
	writeCheckpointTokenComparison(&b, comparison)
	assertContainsAll(t, b.String(),
		"Total tokens: unchanged (200 → 200)",
		"Cache/context replay: unavailable",
		"API calls: up (0 → 3)",
		"Cost share, input: down 20 points (50% → 30%)",
		"Cost share, cache write: unchanged (0% → 0%)",
		"Cost share, output: up 20 points (50% → 70%)",
		"Quality still depends on the task outcome",
	)
}
