package recap

import (
	"strings"
	"testing"
)

func TestRenderStaticRecap_ServerBackedBoth90(t *testing.T) {
	t.Parallel()
	resp := &MeRecapResponse{
		Repo: ptr("entireio/cli"),
		Summary: Summary{
			Me:         SummaryTotals{Sessions: 40, Checkpoints: 92, Tokens: 3_500_000},
			Team:       &SummaryTotals{Sessions: 5, Checkpoints: 6, Tokens: 17_000},
			RepoCount:  1,
			ActiveDays: 14,
		},
		Daily: []DailyCount{
			{Date: "2026-01-24", Count: 0},
			{Date: "2026-01-25", Count: 1},
			{Date: "2026-01-26", Count: 5},
		},
		Agents: map[string]AgentEntry{
			"claude": {
				AgentID:    "claude",
				AgentLabel: "Claude Code",
				Me: AgentAggregate{
					Sessions:    15,
					Checkpoints: 92,
					Tokens:      2_900_000,
					Labels:      []LabelCount{{Label: "bug_fix", Count: 2}},
					Skills:      []SkillCount{{Skill: "code-simplifier", Count: 3}},
					ToolMix:     ToolMix{FileOps: 61, Search: 18, Shell: 15},
				},
				Contributors: &AgentAggregate{
					Sessions:    2,
					Checkpoints: 2,
					Tokens:      1_000,
					Labels:      []LabelCount{{Label: "refactor", Count: 1}},
					Skills:      []SkillCount{{Skill: "session-handoff", Count: 1}},
					ToolMix:     ToolMix{FileOps: 6, Search: 2, Shell: 1},
				},
			},
			"codex": {
				AgentID:    "codex",
				AgentLabel: "Codex",
				Me: AgentAggregate{
					Sessions: 24,
					Tokens:   647_000,
					Skills:   []SkillCount{{Skill: "codex:codex-rescue", Count: 1}},
				},
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: Range90d,
		View:  ViewBoth,
		Agent: "all",
		Width: 78,
	})

	for _, want := range []string{
		"day · week · month · [90d]",
		"agent: [all]",
		"view: you team [both]",
		"Last 90 days",
		"you   40 sessions   92 checkpoints   3.5M tok",
		"team  5 sessions    6 checkpoints    17k tok",
		"1 repo · 14 active days",
		"Activity · 90d",
		"Agents · last 90 days",
		"Claude Code",
		"tokens",
		"2.9M / 1k",
		"team labels",
		"your skills",
		"Labels require server analysis",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderStaticRecap_TeamViewOmitsYouSummary(t *testing.T) {
	t.Parallel()
	resp := &MeRecapResponse{
		Summary: Summary{
			Me:   SummaryTotals{Sessions: 2, Checkpoints: 3, Tokens: 100},
			Team: &SummaryTotals{Sessions: 4, Checkpoints: 5, Tokens: 200},
		},
	}
	got := RenderStaticRecap(resp, RenderOptions{Range: RangeWeek, View: ViewTeam, Agent: AgentAll, Width: 78})
	if strings.Contains(got, "you   ") {
		t.Fatalf("team view should omit you summary:\n%s", got)
	}
	if !strings.Contains(got, "team  4 sessions") {
		t.Fatalf("team view should include team summary:\n%s", got)
	}
}

func TestRenderStaticRecap_AgentFilterSummarizesFilteredAgent(t *testing.T) {
	t.Parallel()
	resp := &MeRecapResponse{
		Summary: Summary{
			Me:         SummaryTotals{Sessions: 99, Checkpoints: 99, Tokens: 99_000},
			Team:       &SummaryTotals{Sessions: 88, Checkpoints: 88, Tokens: 88_000},
			RepoCount:  1,
			ActiveDays: 12,
		},
		Daily: []DailyCount{
			{Date: "2026-04-01", Count: 9},
			{Date: "2026-04-02", Count: 4},
		},
		Agents: map[string]AgentEntry{
			"claude": {
				AgentID:    "claude",
				AgentLabel: "Claude Code",
				Me: AgentAggregate{
					Sessions:    10,
					Checkpoints: 20,
					Tokens:      30_000,
				},
			},
			"codex": {
				AgentID:    "codex",
				AgentLabel: "Codex",
				Me: AgentAggregate{
					Sessions:    2,
					Checkpoints: 3,
					Tokens:      400,
				},
				Contributors: &AgentAggregate{
					Sessions:    4,
					Checkpoints: 5,
					Tokens:      600,
				},
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{Range: RangeWeek, View: ViewBoth, Agent: "codex", Width: 78})
	for _, want := range []string{
		"agent: [codex]",
		"you   2 sessions    3 checkpoints    400 tok",
		"team  4 sessions    5 checkpoints    600 tok",
		"1 agent",
		"Codex",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "99 sessions") || strings.Contains(got, "88 sessions") {
		t.Fatalf("agent-filtered summary should not use all-agent totals:\n%s", got)
	}
	if strings.Contains(got, "Activity · week") {
		t.Fatalf("agent-filtered output should not show all-agent activity:\n%s", got)
	}
	if strings.Contains(got, "12 active days") {
		t.Fatalf("agent-filtered output should not show all-agent active days:\n%s", got)
	}
	if strings.Contains(got, "Claude Code") {
		t.Fatalf("agent-filtered output should omit other agents:\n%s", got)
	}
}

func TestRenderStaticRecap_ColorWhenEnabled(t *testing.T) {
	t.Parallel()
	resp := &MeRecapResponse{
		Summary: Summary{Me: SummaryTotals{Sessions: 1, Checkpoints: 2, Tokens: 300}},
		Daily: []DailyCount{
			{Date: "2026-01-24", Count: 0},
			{Date: "2026-01-25", Count: 1},
			{Date: "2026-01-26", Count: 4},
		},
		Agents: map[string]AgentEntry{
			"codex": {
				AgentID:    "codex",
				AgentLabel: "Codex",
				Me: AgentAggregate{
					Sessions:    1,
					Checkpoints: 2,
					Tokens:      300,
					Labels:      []LabelCount{{Label: "bug_fix", Count: 1}},
					Skills:      []SkillCount{{Skill: "code-simplifier", Count: 1}},
				},
			},
		},
	}

	colored := RenderStaticRecap(resp, RenderOptions{
		Range: Range90d,
		View:  ViewBoth,
		Agent: AgentAll,
		Width: 78,
		Color: true,
	})
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("expected ANSI styling when color is enabled:\n%s", colored)
	}
	if !strings.Contains(colored, "\x1b[38;5;240m░") {
		t.Fatalf("expected empty activity cells to be muted:\n%s", colored)
	}
	if !strings.Contains(colored, "\x1b[1;38;5;214m█") {
		t.Fatalf("expected peak activity cells to be highlighted:\n%s", colored)
	}
	if !strings.Contains(colored, "\x1b[38;5;203m● bug_fix") {
		t.Fatalf("expected labels to use semantic colors:\n%s", colored)
	}
	if !strings.Contains(colored, "\x1b[36mcode-simplifier") {
		t.Fatalf("expected skills to be colorized:\n%s", colored)
	}

	plain := RenderStaticRecap(resp, RenderOptions{
		Range: Range90d,
		View:  ViewBoth,
		Agent: AgentAll,
		Width: 78,
	})
	if strings.Contains(plain, "\x1b[") {
		t.Fatalf("plain output should not contain ANSI styling:\n%s", plain)
	}
}

func ptr(s string) *string {
	return &s
}
