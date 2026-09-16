package models

// Milestone represents a GitHub milestone containing issues.
type Milestone struct {
	ID           string `json:"id"`
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	OpenIssues   int    `json:"open_issues"`
	ClosedIssues int    `json:"closed_issues"`
	URL          string `json:"url,omitempty"`
}

