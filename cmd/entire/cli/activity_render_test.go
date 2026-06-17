package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

const activityTestAgentClaude = "claude"

func TestUniqueCommitAgents_UsesAgentsSlice(t *testing.T) {
	t.Parallel()
	c := userCommit{
		Checkpoints: []userCommitCheckpoint{
			{Agent: "Claude Code", Agents: []string{"Claude Code", "Gemini CLI"}},
		},
	}
	agents := uniqueCommitAgents(c)
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if agents[0] != activityTestAgentClaude || agents[1] != "gemini" {
		t.Errorf("got %v, want [claude gemini]", agents)
	}
}

func TestUniqueCommitAgents_FallsBackToSingularAgent(t *testing.T) {
	t.Parallel()
	c := userCommit{
		Checkpoints: []userCommitCheckpoint{
			{Agent: "Claude Code", Agents: nil},
		},
	}
	agents := uniqueCommitAgents(c)
	if len(agents) != 1 || agents[0] != activityTestAgentClaude {
		t.Errorf("got %v, want [claude] (should fall back to Agent field)", agents)
	}
}

func TestUniqueCommitAgents_FallsBackToSingularAgentEmptySlice(t *testing.T) {
	t.Parallel()
	c := userCommit{
		Checkpoints: []userCommitCheckpoint{
			{Agent: "Amp", Agents: []string{}},
		},
	}
	agents := uniqueCommitAgents(c)
	if len(agents) != 1 || agents[0] != "amp" {
		t.Errorf("got %v, want [amp] (should fall back to Agent field)", agents)
	}
}

func TestUniqueCommitAgents_Dedupes(t *testing.T) {
	t.Parallel()
	c := userCommit{
		Checkpoints: []userCommitCheckpoint{
			{Agent: activityTestAgentClaude, Agents: []string{"Claude Code"}},
			{Agent: activityTestAgentClaude, Agents: []string{"Claude Code"}},
		},
	}
	agents := uniqueCommitAgents(c)
	if len(agents) != 1 {
		t.Errorf("got %d agents, want 1 (should dedupe)", len(agents))
	}
}

func TestUniqueCommitAgents_Empty(t *testing.T) {
	t.Parallel()
	c := userCommit{Checkpoints: nil}
	agents := uniqueCommitAgents(c)
	if len(agents) != 0 {
		t.Errorf("got %v, want empty", agents)
	}
}

func TestRenderStatCards_ContainsAllLabels(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sty := activityStyles{width: 80}
	stats := contributionStats{
		Throughput:    23.2,
		Iteration:     1.4,
		ContinuityH:   0.8,
		Streak:        20,
		CurrentStreak: 17,
	}
	renderStatCards(&buf, sty, stats)
	out := buf.String()

	for _, want := range []string{"THROUGHPUT", "ITERATION", "CONTINUITY", "STREAK"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing label %q", want)
		}
	}
	if !strings.Contains(out, "23.2") {
		t.Errorf("output missing throughput value 23.2")
	}
	if !strings.Contains(out, "0.8") {
		t.Errorf("output missing continuity hours value 0.8")
	}
	if !strings.Contains(out, "17 current") {
		t.Errorf("output missing current streak")
	}
}

func TestRenderStatCards_IterationLabelSaysSession(t *testing.T) {
	t.Parallel()
	// Use wide terminal so descriptions aren't truncated
	var buf bytes.Buffer
	sty := activityStyles{width: 120}
	renderStatCards(&buf, sty, contributionStats{})
	out := buf.String()

	if strings.Contains(out, "steps/checkpoint") {
		t.Error("ITERATION card should say 'sessions/checkpoint', not 'steps/checkpoint'")
	}
	if !strings.Contains(out, "sessions/checkpoint") {
		t.Error("ITERATION card missing 'sessions/checkpoint' label")
	}
}

func TestRenderCommitList_NilCommitMessage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sty := activityStyles{width: 80}
	days := []commitDay{
		{Date: "2026-01-15", Commits: []userCommit{
			{CommitSHA: "abc1234567", CommitMsg: nil, RepoFullName: "org/repo"},
		}},
	}
	renderCommitList(&buf, sty, days)
	out := buf.String()

	if !strings.Contains(out, "(no message)") {
		t.Error("nil commit message should render as '(no message)'")
	}
}

func TestRenderCommitList_SingularPlural(t *testing.T) {
	t.Parallel()

	t.Run("1 file 1 checkpoint", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		sty := activityStyles{width: 80}
		days := []commitDay{
			{Date: "2026-01-15", Commits: []userCommit{
				{
					CommitSHA:    "abc1234567",
					CommitMsg:    strPtr("msg"),
					RepoFullName: "org/repo",
					FilesChanged: 1,
					Checkpoints:  []userCommitCheckpoint{{Agent: activityTestAgentClaude}},
				},
			}},
		}
		renderCommitList(&buf, sty, days)
		out := buf.String()

		if !strings.Contains(out, "1 file") {
			t.Error("should say '1 file'")
		}
		if strings.Contains(out, "1 files") {
			t.Error("should not say '1 files'")
		}
		if !strings.Contains(out, "1 checkpoint") {
			t.Error("should say '1 checkpoint'")
		}
		if !strings.Contains(out, "1 commit") {
			t.Error("should say '1 commit' (singular)")
		}
	})

	t.Run("multiple files and checkpoints", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		sty := activityStyles{width: 80}
		days := []commitDay{
			{Date: "2026-01-15", Commits: []userCommit{
				{
					CommitSHA:    "abc1234567",
					CommitMsg:    strPtr("msg1"),
					RepoFullName: "org/repo",
					FilesChanged: 3,
					Checkpoints:  []userCommitCheckpoint{{}, {}},
				},
				{
					CommitSHA:    "def7654321",
					CommitMsg:    strPtr("msg2"),
					RepoFullName: "org/repo",
					FilesChanged: 2,
				},
			}},
		}
		renderCommitList(&buf, sty, days)
		out := buf.String()

		if !strings.Contains(out, "3 files") {
			t.Error("should say '3 files'")
		}
		if !strings.Contains(out, "2 checkpoints") {
			t.Error("should say '2 checkpoints'")
		}
		if !strings.Contains(out, "2 commits") {
			t.Error("should say '2 commits' (plural)")
		}
	})
}

func TestRenderContributionChart_Empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sty := activityStyles{width: 80}
	renderContributionChart(&buf, sty, nil, nil)
	out := buf.String()
	if !strings.Contains(out, "CONTRIBUTIONS") {
		t.Error("should still show CONTRIBUTIONS header")
	}
	if !strings.Contains(out, "No activity data") {
		t.Error("should show 'No activity data' when empty")
	}
}

func TestRenderContributionChart_MonthAxisWideWidth(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sty := activityStyles{width: 200}
	hourly := []hourlyPoint{
		{Date: "2026-04-01", Hour: 12, Value: 3, AgentID: activityTestAgentClaude},
	}
	repos := []repoContribution{
		{Repo: "org/repo", Total: 1, Agents: map[string]int{activityTestAgentClaude: 1}},
	}

	renderContributionChart(&buf, sty, hourly, repos)
	out := buf.String()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 chart lines, got %d", len(lines))
	}
	axis := lines[1]
	if len(axis) > sty.width+8 {
		t.Fatalf("month axis too wide: got %d chars for width %d", len(axis), sty.width)
	}
}

func TestRenderRepoChart_LimitsToFive(t *testing.T) {
	t.Parallel()
	var repos []repoContribution
	for i := range 8 {
		repos = append(repos, repoContribution{
			Repo:   strings.Repeat("r", i+1),
			Total:  8 - i,
			Agents: map[string]int{activityTestAgentClaude: 8 - i},
		})
	}

	var buf bytes.Buffer
	sty := activityStyles{width: 80}
	renderRepoChart(&buf, sty, repos)
	out := buf.String()

	// Count data lines (excluding the header)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	dataLines := 0
	for _, l := range lines {
		if !strings.Contains(l, "REPOSITORIES") && strings.TrimSpace(l) != "" {
			dataLines++
		}
	}
	if dataLines != 5 {
		t.Errorf("should render max 5 repos, got %d data lines", dataLines)
	}
}

func TestPadOrTruncate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		width int
		want  string
	}{
		{"short padded", "hi", 5, "hi   "},
		{"exact", "hello", 5, "hello"},
		{"truncated", "hello world", 5, "hell…"},
		{"unicode", "héllo", 5, "héllo"},
		{"unicode truncate", "héllo world", 5, "héll…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := padOrTruncate(tt.input, tt.width)
			runes := []rune(got)
			if string(runes) != tt.want {
				t.Errorf("padOrTruncate(%q, %d) = %q, want %q", tt.input, tt.width, got, tt.want)
			}
		})
	}
}

func TestRenderCommitList_UnicodeMessageSafeTruncation(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sty := activityStyles{width: 40}
	msg := "fix café 🔥 renderer alignment"
	days := []commitDay{
		{Date: "2026-01-15", Commits: []userCommit{
			{
				CommitSHA:    "abc1234567",
				CommitMsg:    &msg,
				RepoFullName: "org/repo",
				FilesChanged: 1,
			},
		}},
	}

	renderCommitList(&buf, sty, days)
	out := buf.String()

	if !utf8.ValidString(out) {
		t.Fatal("rendered commit list contains invalid UTF-8")
	}
}

func TestNewStatsStylesWithWidth_RespectsColorFlag(t *testing.T) {
	t.Parallel()
	sty := newActivityStylesWithWidth(80, false)
	if sty.colorEnabled {
		t.Fatal("expected colorEnabled=false")
	}
}

func TestRunStatsTUI_NoColorStyleFlag(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	m := activityModel{
		useColor: shouldUseColor(os.Stdout),
	}
	m.width = 80
	m = m.withViewport()
	m.sty = newActivityStylesWithWidth(m.width, m.useColor)

	if m.useColor {
		t.Fatal("expected NO_COLOR to disable stats TUI colors")
	}
	if m.sty.colorEnabled {
		t.Fatal("expected stats TUI styles to disable colors")
	}
}
