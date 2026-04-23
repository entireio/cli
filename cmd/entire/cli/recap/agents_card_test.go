package recap

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestBuildAgentCards_GroupsByAgent(t *testing.T) {
	t.Parallel()
	now := time.Now()
	sessions := []RecapSession{
		{
			StartedAt: now, LastInteraction: now,
			AgentsUsed: []string{testAgentClaude},
			ModelsUsed: []string{"opus-4-7"},
			Repo:       "entireio/cli",
			TokenUsage: tokenUsageTotal(1000),
			Checkpoints: []RecapCheckpoint{
				{Labels: []string{"bug_fix"}},
			},
		},
		{
			StartedAt: now, LastInteraction: now,
			AgentsUsed: []string{testAgentClaude},
			ModelsUsed: []string{"opus-4-7"},
			Repo:       "entireio/cli",
			TokenUsage: tokenUsageTotal(500),
			Checkpoints: []RecapCheckpoint{
				{Labels: []string{"feature_build"}},
			},
		},
		{
			StartedAt: now, LastInteraction: now,
			AgentsUsed: []string{"codex"},
			Checkpoints: []RecapCheckpoint{
				{Labels: []string{"refactor"}},
			},
		},
	}
	cards := buildAgentCards(sessions)
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (claude-code + codex), got %d", len(cards))
	}
	// claude-code has more sessions → first
	if cards[0].Agent != testAgentClaude {
		t.Errorf("cards[0] = %q, want claude-code", cards[0].Agent)
	}
	if cards[0].MeSessions != 2 {
		t.Errorf("claude-code sessions = %d, want 2", cards[0].MeSessions)
	}
	if cards[0].MeCheckpoints != 2 {
		t.Errorf("claude-code checkpoints = %d, want 2", cards[0].MeCheckpoints)
	}
	if cards[0].MeTokens != 1500 {
		t.Errorf("claude-code tokens = %d, want 1500", cards[0].MeTokens)
	}
	if len(cards[0].MeLabels) != 2 {
		t.Errorf("claude-code labels = %v, want 2", cards[0].MeLabels)
	}
}

func TestAgentCard_BothView_FullData(t *testing.T) {
	t.Parallel()
	c := AgentCard{
		Agent:      "Claude Code",
		MeSessions: 15, MeCheckpoints: 92, MeTokens: 2_900_000,
		ContribSessions: 2, ContribCheckpoints: 2, ContribTokens: 1_000,
		MeModels:       []string{"claude-opus-4-7[1m]"},
		MeRepos:        []RepoInfo{{Repo: "entireio/cli", SessionCount: 15}},
		ContribLabels:  []LabelCount{{"bug_fix", 1}, {"feature_build", 1}, {"refactor", 1}},
		ContribSkills:  []string{"code-simplifier", "session-handoff"},
		ContribToolMix: map[string]int{"fileOps": 61, "search": 18, "shell": 15},
	}
	got := renderAgentCard(c, ViewBoth, 100, NewStyles(false))
	for _, want := range []string{
		"Claude Code", "tokens", "sessions", "checkpoints",
		"2.9M / 1k", "15 / 2", "92 / 2",
		"team labels", "bug_fix", "feature_build",
		"team skills", "code-simplifier",
		"team tool mix", "fileOps 61%",
		"your models", "claude-opus-4-7[1m]",
		"your repos", "entireio/cli (15)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("both-view card missing %q; got:\n%s", want, got)
		}
	}
}

func TestAgentCard_TeamView_DropsYourRows(t *testing.T) {
	t.Parallel()
	c := AgentCard{
		Agent:           "Claude Code",
		ContribSessions: 2, ContribCheckpoints: 2, ContribTokens: 1_000,
		ContribLabels:  []LabelCount{{"bug_fix", 1}},
		ContribSkills:  []string{"code-simplifier"},
		ContribToolMix: map[string]int{"fileOps": 61, "search": 18},
		MeModels:       []string{"claude-opus-4-7[1m]"}, // present but must hide
		MeRepos:        []RepoInfo{{Repo: "entireio/cli", SessionCount: 15}},
	}
	got := renderAgentCard(c, ViewContributors, 100, NewStyles(false))
	if strings.Contains(got, "your models") || strings.Contains(got, "your repos") {
		t.Error("team-view should hide your-side blocks")
	}
	if strings.Contains(got, "team labels") {
		t.Error("team-view should omit 'team' prefix (single-side mode)")
	}
	if !strings.Contains(got, "bug_fix") {
		t.Errorf("team-view should show labels; got:\n%s", got)
	}
}

func TestAgentCard_YouView_DropsTeamRows(t *testing.T) {
	t.Parallel()
	c := AgentCard{
		Agent:      "Codex",
		MeSessions: 24, MeTokens: 647_000,
		MeModels:      []string{"gpt-5.4"},
		MeRepos:       []RepoInfo{{Repo: "entireio/cli", SessionCount: 24}},
		ContribLabels: []LabelCount{{"investigation", 1}}, // present but must be hidden
	}
	got := renderAgentCard(c, ViewMe, 100, NewStyles(false))
	if strings.Contains(got, "team labels") {
		t.Error("you-view should hide team labels")
	}
	if strings.Contains(got, "your models") {
		t.Error("you-view should omit 'your' prefix")
	}
	if !strings.Contains(got, "gpt-5.4") {
		t.Errorf("you-view should show models; got:\n%s", got)
	}
}

func TestAgentCard_BothView_DropsZeroRows(t *testing.T) {
	t.Parallel()
	c := AgentCard{
		Agent:      "Codex",
		MeSessions: 24, MeTokens: 647_000,
		MeCheckpoints: 0, ContribCheckpoints: 0, // both zero → row drops
		MeModels: []string{"gpt-5.4"},
	}
	got := renderAgentCard(c, ViewBoth, 100, NewStyles(false))
	if strings.Contains(got, "checkpoints") {
		t.Errorf("checkpoints row should drop when both zero; got:\n%s", got)
	}
	if !strings.Contains(got, "tokens") {
		t.Error("tokens row should render when you has value")
	}
}

func TestRenderAgentsView_PanelHeader_BothView(t *testing.T) {
	t.Parallel()
	cards := []AgentCard{{Agent: "Claude Code", MeSessions: 1}}
	got := renderAgentsView(cards, ViewBoth, 80, NewStyles(false))
	if !strings.Contains(got, "you") || !strings.Contains(got, "team") {
		t.Errorf("both-view panel should have you/team legend; got:\n%s", got)
	}
}

func TestRenderAgentsView_PanelHeader_SingleView_NoLegend(t *testing.T) {
	t.Parallel()
	cards := []AgentCard{{Agent: "Claude Code", MeSessions: 1}}
	got := renderAgentsView(cards, ViewMe, 80, NewStyles(false))
	// Legend only appears in both view.
	if strings.Contains(got, "███") && strings.Contains(got, "▒") {
		t.Errorf("single-view panel should not have legend; got:\n%s", got)
	}
}

func TestRenderAgentsView_SortByCombinedSessions_AlphabeticalTieBreak(t *testing.T) {
	t.Parallel()
	// Two agents tied on combined sessions — alphabetical tie-break means
	// "Aardvark" comes before "Zebra".
	cards := []AgentCard{
		{Agent: "Zebra", MeSessions: 5, ContribSessions: 0},
		{Agent: "Aardvark", MeSessions: 3, ContribSessions: 2},
	}
	got := renderAgentsView(cards, ViewBoth, 80, NewStyles(false))
	aIdx := strings.Index(got, "Aardvark")
	zIdx := strings.Index(got, "Zebra")
	if aIdx < 0 || zIdx < 0 || aIdx >= zIdx {
		t.Errorf("tie-break failed: Aardvark should come before Zebra; got\n%s", got)
	}
}

func TestRenderAgentsView_EmptyShowsPlaceholder(t *testing.T) {
	t.Parallel()
	got := renderAgentsView(nil, ViewBoth, 80, NewStyles(false))
	if !strings.Contains(got, "no agent activity") {
		t.Errorf("empty panel should show placeholder; got:\n%s", got)
	}
}

func TestCycleMode_Progression(t *testing.T) {
	t.Parallel()
	steps := []ViewMode{ViewBoth, ViewMe, ViewContributors, ViewBoth}
	cur := ViewBoth
	for i := 1; i < len(steps); i++ {
		cur = cycleMode(cur)
		if cur != steps[i] {
			t.Errorf("step %d: got %q, want %q", i, cur, steps[i])
		}
	}
}

func TestAgentCard_NarrowWidth_ReadoutOnly(t *testing.T) {
	t.Parallel()
	c := AgentCard{
		Agent:           "Claude Code",
		MeTokens:        2_900_000,
		ContribTokens:   1_000,
		MeSessions:      15,
		ContribSessions: 2,
	}
	// innerWidth=36 → barWidth=36-12-14-4=6, below barMinWidth=12, bar drops
	narrow := renderAgentCard(c, ViewBoth, 36, NewStyles(false))
	if strings.Contains(narrow, "█") || strings.Contains(narrow, "▒") {
		t.Errorf("narrow card should drop bars; got:\n%s", narrow)
	}
	if !strings.Contains(narrow, "2.9M / 1k") {
		t.Errorf("narrow card should still show readout; got:\n%s", narrow)
	}
}

// tokenUsageTotal is a tiny test helper: builds a TokenUsage whose
// Input+Output sum to total so tests can assert token aggregation.
func tokenUsageTotal(total int) *agent.TokenUsage {
	return &agent.TokenUsage{InputTokens: total, OutputTokens: 0}
}
