package recap

// A repo's team rollups live on its processing cell, while the caller's own
// activity lives on their home cell. When the two differ, the client asks both
// for /me/recap and keeps each side from the cell that holds it; entire-api
// does not merge across cells.

// ClearTeam removes the team side of a recap, so it renders as unavailable
// rather than as zeros. Used for a home-cell response when the repo's team data
// lives on another cell.
func ClearTeam(r *MeRecapResponse) {
	r.Summary.Team = nil
	r.Summary.Transcripts.Team = nil
	r.Contributors = nil
	for key, entry := range r.Agents {
		entry.Contributors = nil
		r.Agents[key] = entry
	}
}

// MergeTeam replaces home's team side with team's. home supplies the personal
// side and the window; team is the repo processing cell's answer for the same
// window. A nil team, or one without a team side (the caller is not a member
// there), leaves home with no team side.
//
// Each cell folds custom agent names into "unknown" on its own, so with more
// than the server's custom-agent limit the two sides can name an agent
// differently; the union then shows both rows.
func MergeTeam(home, team *MeRecapResponse) {
	ClearTeam(home)
	if team == nil || team.Summary.Team == nil {
		return
	}
	home.Summary.Team = team.Summary.Team
	home.Summary.Transcripts.Team = team.Summary.Transcripts.Team
	for key, teamEntry := range team.Agents {
		if teamEntry.Contributors == nil {
			continue
		}
		if home.Agents == nil {
			home.Agents = map[string]AgentEntry{}
		}
		entry, ok := home.Agents[key]
		if !ok {
			entry = AgentEntry{AgentID: teamEntry.AgentID, AgentLabel: teamEntry.AgentLabel}
		}
		entry.Contributors = teamEntry.Contributors
		home.Agents[key] = entry
	}
	if team.Contributors != nil {
		// The server's totals add the caller's own activity to the team's
		// (entire-api buildRecap); the team cell has none of the caller's rows.
		home.Contributors = &ContribSummary{
			DistinctUsers:    team.Contributors.DistinctUsers,
			TotalTokens:      home.Summary.Me.Tokens + team.Summary.Team.Tokens,
			TotalCheckpoints: home.Summary.Me.Checkpoints + team.Summary.Team.Checkpoints,
		}
	}
}
