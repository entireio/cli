package recap

import (
	"testing"
)

func TestMergeContributors_PopulatesFromAgentActivity(t *testing.T) {
	t.Parallel()
	act := &agentActivityResponse{
		Daily: []struct {
			Date         string `json:"date"`
			Agent        string `json:"agent"`
			Count        int    `json:"count"`
			Tokens       int    `json:"tokens"`
			InputTokens  int    `json:"inputTokens"`
			OutputTokens int    `json:"outputTokens"`
		}{
			{Date: "2026-04-20", Agent: "claude-code", Count: 5, Tokens: 100000},
			{Date: "2026-04-21", Agent: "claude-code", Count: 3, Tokens: 50000},
			{Date: "2026-04-21", Agent: "codex", Count: 2, Tokens: 30000},
		},
	}
	data := mergeContributors("org/repo", act, nil, nil)
	if c := data.ByAgent["claude-code"]; c == nil || c.TotalCount != 8 || c.Tokens != 150000 {
		t.Errorf("claude-code: %+v, want count=8 tokens=150000", c)
	}
	if c := data.ByAgent["codex"]; c == nil || c.TotalCount != 2 {
		t.Errorf("codex: %+v, want count=2", c)
	}
}

func TestMergeContributors_CountsDistinctFromContributorAgents(t *testing.T) {
	t.Parallel()
	agents := &contributorAgentsResponse{
		Contributors: []struct {
			Username     *string        `json:"username"`
			GithubID     *int           `json:"github_id"`
			TotalCommits int            `json:"total_commits"`
			Agents       map[string]int `json:"agents"`
			Untracked    int            `json:"untracked"`
		}{
			{Agents: map[string]int{"claude-code": 5, "codex": 2}},
			{Agents: map[string]int{"claude-code": 3}},
			{Agents: map[string]int{"codex": 0, "claude-code": 1}}, // codex 0 excluded
		},
	}
	data := mergeContributors("org/repo", nil, agents, nil)
	if c := data.ByAgent["claude-code"]; c == nil || c.DistinctContribs != 3 {
		t.Errorf("claude-code distinct: %+v, want 3", c)
	}
	if c := data.ByAgent["codex"]; c == nil || c.DistinctContribs != 1 {
		t.Errorf("codex distinct: %+v, want 1", c)
	}
}

func TestMergeContributors_EmptyInputs(t *testing.T) {
	t.Parallel()
	data := mergeContributors("org/repo", nil, nil, nil)
	if data == nil {
		t.Fatal("expected non-nil data even with all-nil inputs")
	}
	if len(data.ByAgent) != 0 {
		t.Errorf("expected empty ByAgent, got %v", data.ByAgent)
	}
	if data.Repo != "org/repo" {
		t.Errorf("Repo = %q, want org/repo", data.Repo)
	}
}

func TestApplyContributors_FillsAgentCardFields(t *testing.T) {
	t.Parallel()
	cards := []AgentCard{
		{Agent: "claude-code"},
		{Agent: "codex"},
	}
	data := &ContributorsData{
		ByAgent: map[string]*AgentContrib{
			"claude-code": {TotalCount: 89, Tokens: 1_200_000, DistinctContribs: 4},
		},
	}
	applyContributors(cards, data)
	if cards[0].ContribSessions != 89 || cards[0].ContribTokens != 1_200_000 || cards[0].ContribCount != 4 {
		t.Errorf("claude-code card not filled: %+v", cards[0])
	}
	if cards[1].ContribSessions != 0 {
		t.Errorf("codex card should stay zero (no data): %+v", cards[1])
	}
}

func TestApplyContributors_NilDataIsNoop(t *testing.T) {
	t.Parallel()
	cards := []AgentCard{{Agent: "claude-code", ContribSessions: 0}}
	applyContributors(cards, nil)
	if cards[0].ContribSessions != 0 {
		t.Errorf("nil data should not modify cards; got %+v", cards[0])
	}
}
