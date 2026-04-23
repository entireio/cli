package recap

import (
	"strings"
	"testing"
)

func TestRenderSummaryBand_AllTopSignals(t *testing.T) {
	t.Parallel()
	s := SummaryBand{
		RangeLabel:      "Last 90 days",
		YouSessions:     40,
		YouCheckpoints:  92,
		YouTokens:       3_500_000,
		TeamSessions:    5,
		TeamCheckpoints: 6,
		TeamTokens:      17_000,
		TopAgent:        "Codex",
		TopSkill:        "code-simplifier",
		TopLabel:        "bug_fix",
		TopModel:        "claude-opus-4-7[1m]",
		AgentCount:      5,
		RepoCount:       1,
		ActiveDays:      14,
	}
	got := renderSummaryBand(s, NewStyles(false))
	for _, want := range []string{
		"Last 90 days",
		"you", "40 sessions", "92 checkpoints", "3.5M tok",
		"team", "5 sessions", "6 checkpoints", "17k tok",
		"top", "Codex", "code-simplifier", "bug_fix", "claude-opus-4-7[1m]",
		"5 agents", "1 repo", "14 active days",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q; got:\n%s", want, got)
		}
	}
}

func TestRenderSummaryBand_TopLineReflows(t *testing.T) {
	t.Parallel()
	// Only top agent + top model have data; skill + label empty.
	s := SummaryBand{
		RangeLabel:  "Last 90 days",
		YouSessions: 40,
		TopAgent:    "Codex",
		TopModel:    "gpt-5.4",
		AgentCount:  2,
		RepoCount:   1,
		ActiveDays:  6,
	}
	got := renderSummaryBand(s, NewStyles(false))
	if !strings.Contains(got, "top  Codex") && !strings.Contains(got, "top Codex") {
		t.Errorf("top line should start with Codex; got:\n%s", got)
	}
	if !strings.Contains(got, "gpt-5.4") {
		t.Errorf("top line should include gpt-5.4; got:\n%s", got)
	}
	// Empty signals must not render placeholders.
	if strings.Contains(got, "—") {
		t.Errorf("top line should omit empties, not render —; got:\n%s", got)
	}
}

func TestRenderSummaryBand_TopLineOmittedWhenAllEmpty(t *testing.T) {
	t.Parallel()
	s := SummaryBand{
		RangeLabel:     "Last 90 days",
		YouSessions:    2,
		YouCheckpoints: 3,
		YouTokens:      12_000,
		AgentCount:     1,
		RepoCount:      1,
		ActiveDays:     2,
	}
	got := renderSummaryBand(s, NewStyles(false))
	if strings.Contains(got, "top ") {
		t.Errorf("top line should not render when all signals empty; got:\n%s", got)
	}
}
