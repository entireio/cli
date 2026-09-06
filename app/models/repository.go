package models

// Repository represents a git code repository managed by Entire Checkpoint Intelligence.
type Repository struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	URL           string `json:"url"`
	LocalPath     string `json:"local_path"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
}
