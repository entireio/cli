package recap

import "testing"

func teamTotals(sessions, checkpoints, tokens int) *SummaryTotals {
	return &SummaryTotals{Sessions: sessions, Checkpoints: checkpoints, Tokens: tokens}
}

func TestMergeTeam_TakesEachSideFromItsCell(t *testing.T) {
	t.Parallel()
	home := &MeRecapResponse{
		Summary: Summary{
			Me:         SummaryTotals{Sessions: 3, Checkpoints: 4, Tokens: 100},
			Team:       teamTotals(0, 0, 0), // the home cell's empty view of the repo
			ActiveDays: 2,
		},
		Agents: map[string]AgentEntry{
			"claude-code": {AgentID: "claude-code", AgentLabel: "Claude Code", Me: AgentAggregate{Sessions: 3}, Contributors: &AgentAggregate{}},
		},
		Daily: []DailyCount{{Date: "2026-10-22", Count: 3}},
	}
	team := &MeRecapResponse{
		Summary: Summary{
			Team:        teamTotals(40, 50, 9000),
			Transcripts: TranscriptSummary{Team: &TranscriptStatus{Pending: 2}},
		},
		Agents: map[string]AgentEntry{
			"claude-code": {AgentID: "claude-code", AgentLabel: "Claude Code", Contributors: &AgentAggregate{Sessions: 30}},
			"codex":       {AgentID: "codex", AgentLabel: "Codex", Contributors: &AgentAggregate{Sessions: 10}},
		},
		Contributors: &ContribSummary{DistinctUsers: 7, TotalTokens: 9000, TotalCheckpoints: 50},
	}

	MergeTeam(home, team)

	if home.Summary.Me.Sessions != 3 || home.Summary.ActiveDays != 2 || len(home.Daily) != 1 {
		t.Errorf("personal side changed: %+v daily=%v", home.Summary, home.Daily)
	}
	if home.Summary.Team == nil || home.Summary.Team.Sessions != 40 {
		t.Errorf("summary.team = %+v, want the team cell's", home.Summary.Team)
	}
	if home.Summary.Transcripts.Team == nil || home.Summary.Transcripts.Team.Pending != 2 {
		t.Errorf("transcripts.team = %+v, want the team cell's", home.Summary.Transcripts.Team)
	}
	cc := home.Agents["claude-code"]
	if cc.Me.Sessions != 3 || cc.Contributors == nil || cc.Contributors.Sessions != 30 {
		t.Errorf("claude-code = %+v, want me from home and contributors from team", cc)
	}
	codex, ok := home.Agents["codex"]
	if !ok || codex.Me.Sessions != 0 || codex.Contributors == nil || codex.Contributors.Sessions != 10 || codex.AgentLabel != "Codex" {
		t.Errorf("team-only agent = %+v (present %v), want a zero me side", codex, ok)
	}
	want := ContribSummary{DistinctUsers: 7, TotalTokens: 100 + 9000, TotalCheckpoints: 4 + 50}
	if home.Contributors == nil || *home.Contributors != want {
		t.Errorf("contributors = %+v, want %+v", home.Contributors, want)
	}
}

func TestMergeTeam_NoTeamSideClearsHomeTeam(t *testing.T) {
	t.Parallel()
	for name, team := range map[string]*MeRecapResponse{
		"team cell failed":              nil,
		"not a member on the team cell": {Summary: Summary{Team: nil}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := &MeRecapResponse{
				Summary:      Summary{Team: teamTotals(0, 0, 0), Transcripts: TranscriptSummary{Team: &TranscriptStatus{}}},
				Agents:       map[string]AgentEntry{"claude-code": {Contributors: &AgentAggregate{}}},
				Contributors: &ContribSummary{},
			}
			MergeTeam(home, team)
			if home.Summary.Team != nil || home.Summary.Transcripts.Team != nil || home.Contributors != nil || home.Agents["claude-code"].Contributors != nil {
				t.Errorf("team side not cleared: summary=%+v agents=%+v contributors=%+v", home.Summary, home.Agents, home.Contributors)
			}
		})
	}
}
