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
			Checkpoints: []RecapCheckpoint{
				{Labels: []string{"bug_fix"}, TokenUsage: tokenUsageTotal(1000)},
			},
		},
		{
			StartedAt: now, LastInteraction: now,
			AgentsUsed: []string{testAgentClaude},
			ModelsUsed: []string{"opus-4-7"},
			Repo:       "entireio/cli",
			Checkpoints: []RecapCheckpoint{
				{Labels: []string{"feature_build"}, TokenUsage: tokenUsageTotal(500)},
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

func TestRenderAgentsView_BothModeShowsColumns(t *testing.T) {
	t.Parallel()
	cards := []AgentCard{
		{
			Agent:           testAgentClaude,
			MeSessions:      12,
			MeCheckpoints:   47,
			MeTokens:        142000,
			MeLabels:        []LabelCount{{Label: "bug_fix", Count: 5}},
			ContribSessions: 89,
			ContribTokens:   1_200_000,
			ContribCount:    4,
		},
	}
	out := renderAgentsView(cards, ViewBoth, NewStyles(false))
	for _, want := range []string{testAgentClaude, "me", "contributors", "12", "89"} {
		if !strings.Contains(out, want) {
			t.Errorf("Both mode missing %q:\n%s", want, out)
		}
	}
}

func TestRenderAgentsView_MeModeHidesContrib(t *testing.T) {
	t.Parallel()
	cards := []AgentCard{{
		Agent: testAgentClaude, MeSessions: 12,
		ContribSessions: 89,
	}}
	out := renderAgentsView(cards, ViewMe, NewStyles(false))
	if strings.Contains(out, "contributors") {
		t.Errorf("me mode shouldn't mention contributors:\n%s", out)
	}
	if !strings.Contains(out, "12") {
		t.Errorf("me mode should show my count:\n%s", out)
	}
}

func TestRenderAgentsView_EmptyCards(t *testing.T) {
	t.Parallel()
	out := renderAgentsView(nil, ViewBoth, NewStyles(false))
	if !strings.Contains(out, "no agents") {
		t.Errorf("empty cards should show placeholder:\n%s", out)
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

// tokenUsageTotal is a tiny test helper: builds a TokenUsage whose
// Input+Output sum to total so tests can assert token aggregation.
func tokenUsageTotal(total int) *agent.TokenUsage {
	return &agent.TokenUsage{InputTokens: total, OutputTokens: 0}
}
