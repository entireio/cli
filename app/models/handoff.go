package models

// Handoff represents an actionable developer or agent handoff briefing generated from checkpoint context.
type Handoff struct {
	ID                    string   `json:"id"`
	OriginalIntent        string   `json:"original_intent"`
	CompletedWork         []string `json:"completed_work"`
	RemainingWork         []string `json:"remaining_work"`
	ImportantFiles        []string `json:"important_files"`
	Risks                 []string `json:"risks"`
	LastCheckpoint        string   `json:"last_checkpoint"`
	RecommendedNextAction string   `json:"recommended_next_action"`
}
