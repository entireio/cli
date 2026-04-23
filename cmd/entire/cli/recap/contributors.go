package recap

// ContributorsData is the per-agent roll-up used by the contributors column.
// Keyed by canonical agent name (e.g. "claude-code"). Populated by
// ContributorsFromMeRecap in me_recap.go — /api/v1/me/recap is the source
// of truth. Empty map = no data (not logged in, repo not tracked, or empty
// window).
type ContributorsData struct {
	Repo    string
	ByAgent map[string]*AgentContrib
}

// AgentContrib is what the contributors column shows per agent. Mirrors the
// shape of the "me" side of AgentCard but populated from server data.
type AgentContrib struct {
	TotalCount       int
	Tokens           int
	DistinctContribs int

	Labels     []LabelCount
	Skills     []string
	MCPServers []string
	ToolMix    map[string]int // category (shell/fileOps/…) → count
}
