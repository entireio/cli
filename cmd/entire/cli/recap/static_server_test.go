package recap

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenderStaticRecap_ServerBackedBoth90(t *testing.T) {
	t.Parallel()
	resp := &MeRecapResponse{
		Repo:  ptr("entireio/cli"),
		Since: "2026-05-02T04:00:00Z",
		Until: "2026-05-09T04:00:00Z",
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
		Range:    Range90d,
		View:     ViewBoth,
		Agent:    "all",
		Width:    78,
		Location: time.FixedZone("EDT", -4*60*60),
	})

	for _, want := range []string{
		"day · week · month · [90d]",
		"agent: [all]",
		"view: you team [both]",
		"Last 90 days",
		"window May 2, 2026 00:00 EDT - May 9, 2026 00:00 EDT",
		"you   40 sessions   92 checkpoints   3.5M tok",
		"team  5 sessions    6 checkpoints    17k tok",
		"repo entireio/cli · 14 active days",
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

func TestRenderStaticRecap_ListsMultipleRepoNames(t *testing.T) {
	t.Parallel()

	resp := &MeRecapResponse{
		Repos: []string{"org/a", "org/b", "org/c", "org/d"},
		Summary: Summary{
			Me:         SummaryTotals{Sessions: 1},
			RepoCount:  4,
			ActiveDays: 2,
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewBoth,
		Agent: AgentAll,
		Width: 90,
	})

	if !strings.Contains(got, "repos org/a, org/b, org/c +1 more · 2 active days") {
		t.Fatalf("output should list repo names with overflow count:\n%s", got)
	}
}

func TestRenderStaticRecap_WrapsContextLineWithLongRepoNames(t *testing.T) {
	t.Parallel()

	// Three long repo names so the joined context line (agents · repos … ·
	// active days) exceeds the available content width at minWidth (60).
	// Without wrap-aware rendering, the line would tear the box border.
	// Names chosen so each is under the content width on its own (~56 at
	// width 60) — the wrap point is whitespace between names.
	resp := &MeRecapResponse{
		Repos: []string{
			"entireio/very-long-monorepo-name",
			"entireio/another-very-long-repo",
			"entireio/yet-another-long-one",
		},
		Summary: Summary{
			Me:         SummaryTotals{Sessions: 1},
			RepoCount:  3,
			ActiveDays: 5,
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewBoth,
		Agent: AgentAll,
		Width: 60,
	})

	// Positive assertion: all three repos and the active-days segment are
	// rendered somewhere — without this the test would pass if context
	// rendering were deleted.
	for _, want := range append([]string{}, resp.Repos...) {
		if !strings.Contains(got, want) {
			t.Fatalf("output should include repo %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "5 active days") {
		t.Fatalf("output should include active-days segment:\n%s", got)
	}
	// Width assertion: nothing tears the box at width 60. This is the
	// regression guard — pre-fix, the joined context line was rendered
	// verbatim and spilled past the right border.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "│") && displayLen(line) > 60 {
			t.Fatalf("summary box line should fit width 60, got width %d:\n%s\n\nfull output:\n%s",
				displayLen(line), line, got)
		}
	}
}

func TestRenderStaticRecap_ShowsUnavailableTranscriptNote(t *testing.T) {
	t.Parallel()

	resp := &MeRecapResponse{
		Summary: Summary{
			Me: SummaryTotals{Sessions: 1, Checkpoints: 3},
			Transcripts: TranscriptSummary{
				Me: TranscriptStatus{Failed: 1, Pending: 1, Empty: 1},
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewYou,
		Agent: AgentAll,
		Width: 90,
	})

	if !strings.Contains(got, "3 unavailable transcripts") {
		t.Fatalf("output should mention unavailable transcript count:\n%s", got)
	}
	if !strings.Contains(got, "1 failed, 1 pending, 1 empty") {
		t.Fatalf("output should include transcript status breakdown:\n%s", got)
	}
	if !strings.Contains(got, "session totals may be lower") {
		t.Fatalf("output should explain the mismatch risk:\n%s", got)
	}
}

func TestRenderStaticRecap_WrapsUnavailableTranscriptNote(t *testing.T) {
	t.Parallel()

	// Counts deliberately chosen so the detail line exceeds the available
	// content width at minWidth (60). Available width inside the summary box
	// is width - 4 (box borders + 2-space indent), so contentWidth at 60 is
	// 56 chars; the detail line below renders to ~70 chars and must wrap.
	resp := &MeRecapResponse{
		Summary: Summary{
			Me: SummaryTotals{Sessions: 6, Checkpoints: 38},
			Transcripts: TranscriptSummary{
				Me: TranscriptStatus{Failed: 12345, Pending: 6789, Empty: 99999},
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewYou,
		Agent: AgentAll,
		Width: 60,
	})

	// Positive assertion: the note is actually rendered. Without this check
	// the test would pass even if the note function were deleted.
	wantTotal := 12345 + 6789 + 99999 // 119_133
	if !strings.Contains(got, fmt.Sprintf("%d unavailable transcripts", wantTotal)) {
		t.Fatalf("output should mention total unavailable transcript count %d:\n%s", wantTotal, got)
	}
	// "session totals may be lower" intentionally split — the whole point of
	// this test is that wrapping is happening, so the contiguous substring
	// won't survive the wrap boundary.
	for _, want := range []string{"failed", "pending", "empty", "session totals", "may be lower"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output should include %q:\n%s", want, got)
		}
	}
	// Wrapping assertion: the detail line should wrap onto a continuation
	// line. Without wrapping, the note would produce exactly two box lines
	// (headline + detail). With wrapping at width 60 and these counts, the
	// detail spans the wrap boundary, producing at least three.
	noteFragments := []string{"unavailable", "failed", "pending", "empty", "session totals", "may be lower"}
	noteLines := 0
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "│") {
			continue
		}
		for _, fragment := range noteFragments {
			if strings.Contains(line, fragment) {
				noteLines++
				break
			}
		}
	}
	if noteLines < 3 {
		t.Fatalf("note should wrap onto at least 3 box lines at width 60, got %d:\n%s", noteLines, got)
	}
	// Width assertion: nothing tears the box at width 60.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "│") && displayLen(line) > 60 {
			t.Fatalf("summary box line should fit width 60, got width %d:\n%s\n\nfull output:\n%s", displayLen(line), line, got)
		}
	}
}

func TestRenderStaticRecap_TranscriptNoteSumsMeAndTeamInViewBoth(t *testing.T) {
	t.Parallel()

	// Load-bearing summing: ViewBoth must aggregate Me + Team transcript
	// counts so the headline matches the sessions visible in that view.
	resp := &MeRecapResponse{
		Summary: Summary{
			Me:   SummaryTotals{Sessions: 1, Checkpoints: 3},
			Team: &SummaryTotals{Sessions: 2, Checkpoints: 4},
			Transcripts: TranscriptSummary{
				Me:   TranscriptStatus{Failed: 1, Pending: 2, Empty: 3},
				Team: &TranscriptStatus{Failed: 4, Pending: 5, Empty: 6},
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewBoth,
		Agent: AgentAll,
		Width: 90,
	})

	// 1+2+3 + 4+5+6 = 21
	if !strings.Contains(got, "21 unavailable transcripts") {
		t.Fatalf("ViewBoth should sum Me+Team transcripts (expected 21):\n%s", got)
	}
	if !strings.Contains(got, "5 failed, 7 pending, 9 empty") {
		t.Fatalf("ViewBoth should sum each transcript status across Me+Team:\n%s", got)
	}
}

func TestRenderStaticRecap_TranscriptNoteHandlesNilTeamInViewTeam(t *testing.T) {
	t.Parallel()

	// ViewTeam with Transcripts.Team == nil must not panic and must omit
	// the diagnostics note entirely (there is nothing to report).
	resp := &MeRecapResponse{
		Summary: Summary{
			Me:   SummaryTotals{Sessions: 1, Checkpoints: 3},
			Team: &SummaryTotals{Sessions: 0, Checkpoints: 0},
			Transcripts: TranscriptSummary{
				Me:   TranscriptStatus{Failed: 9, Pending: 9, Empty: 9},
				Team: nil,
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewTeam,
		Agent: AgentAll,
		Width: 90,
	})

	if strings.Contains(got, "unavailable transcript") {
		t.Fatalf("ViewTeam with nil Team transcripts should omit the note:\n%s", got)
	}
}

func TestRenderStaticRecap_TranscriptNoteOmittedWhenAllZero(t *testing.T) {
	t.Parallel()

	resp := &MeRecapResponse{
		Summary: Summary{
			Me: SummaryTotals{Sessions: 1, Checkpoints: 3},
			Transcripts: TranscriptSummary{
				Me: TranscriptStatus{Failed: 0, Pending: 0, Empty: 0},
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewYou,
		Agent: AgentAll,
		Width: 90,
	})

	if strings.Contains(got, "unavailable transcript") {
		t.Fatalf("zero transcript counts should omit the note entirely:\n%s", got)
	}
}

func TestRenderStaticRecap_TranscriptNoteSingularLabel(t *testing.T) {
	t.Parallel()

	resp := &MeRecapResponse{
		Summary: Summary{
			Me: SummaryTotals{Sessions: 1, Checkpoints: 3},
			Transcripts: TranscriptSummary{
				Me: TranscriptStatus{Failed: 1},
			},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewYou,
		Agent: AgentAll,
		Width: 90,
	})

	if !strings.Contains(got, "1 unavailable transcript") {
		t.Fatalf("singular total should use singular label:\n%s", got)
	}
	if strings.Contains(got, "1 unavailable transcripts") {
		t.Fatalf("singular total should not use plural label:\n%s", got)
	}
}

func TestRenderStaticRecap_WindowSkippedForInvalidTimestamps(t *testing.T) {
	t.Parallel()

	resp := &MeRecapResponse{
		Since: "not-a-real-timestamp",
		Until: "2026-05-09T04:00:00Z",
		Summary: Summary{
			Me: SummaryTotals{Sessions: 1},
		},
	}

	got := RenderStaticRecap(resp, RenderOptions{
		Range: RangeWeek,
		View:  ViewYou,
		Agent: AgentAll,
		Width: 78,
	})

	if strings.Contains(got, "window ") {
		t.Fatalf("invalid timestamps should skip the window line entirely:\n%s", got)
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
