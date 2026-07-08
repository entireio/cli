// Package trail provides types and helpers for managing trail metadata.
// Trails are branch-centric work-tracking abstractions served by the core
// API. They answer "why/what" (human intent) while checkpoints answer
// "how/when" (machine snapshots).
package trail

import (
	"strings"
	"time"
)

// ID is a 12-character hex identifier for trails.
type ID string

// EmptyID represents an unset or invalid trail ID.
const EmptyID ID = ""

// String returns the trail ID as a string.
func (id ID) String() string {
	return string(id)
}

// IsEmpty returns true if the trail ID is empty or unset.
func (id ID) IsEmpty() bool {
	return id == EmptyID
}

// Status represents the lifecycle status of a trail.
type Status string

// The status set mirrors the server's repo_trails check constraint
// ('draft', 'open', 'merged', 'closed'). The former in_progress and
// in_review statuses were folded into open server-side.
const (
	StatusDraft  Status = "draft"
	StatusOpen   Status = "open"
	StatusMerged Status = "merged"
	StatusClosed Status = "closed"
)

// ValidStatuses returns all valid trail statuses in lifecycle order.
func ValidStatuses() []Status {
	return []Status{
		StatusDraft,
		StatusOpen,
		StatusMerged,
		StatusClosed,
	}
}

// IsValid returns true if the status is a recognized trail status.
func (s Status) IsValid() bool {
	for _, vs := range ValidStatuses() {
		if s == vs {
			return true
		}
	}
	return false
}

// ReviewerStatus represents the review status for a reviewer.
type ReviewerStatus string

const (
	ReviewerPending          ReviewerStatus = "pending"
	ReviewerApproved         ReviewerStatus = "approved"
	ReviewerChangesRequested ReviewerStatus = "changes_requested"
)

// Reviewer represents a reviewer assigned to a trail.
type Reviewer struct {
	Login  string         `json:"login"`
	Status ReviewerStatus `json:"status"`
}

// Author identifies the user who created a trail.
// On the wire the whole object may be null when the original author can no
// longer be resolved (e.g. the GitHub user no longer exists), and login may
// independently be null while the id is retained.
type Author struct {
	ID    string  `json:"id"`
	Login *string `json:"login"`
}

// Metadata represents the metadata for a trail, matching the web PR format.
type Metadata struct {
	Number    int        `json:"number,omitempty"`
	TrailID   ID         `json:"trail_id"`
	URL       string     `json:"url,omitempty"`
	Branch    string     `json:"branch"`
	Base      string     `json:"base"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Status    Status     `json:"status"`
	Phase     string     `json:"phase,omitempty"`
	Author    *Author    `json:"author"`
	Assignees []string   `json:"assignees"`
	Labels    []string   `json:"labels"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
}

// AuthorLogin returns the trail author's login, or an empty string if the
// author is unknown (object null) or the login is null.
func (m *Metadata) AuthorLogin() string {
	if m == nil || m.Author == nil || m.Author.Login == nil {
		return ""
	}
	return *m.Author.Login
}

// commonBranchPrefixes are stripped from branch names when humanizing.
var commonBranchPrefixes = []string{
	"feature/",
	"fix/",
	"bugfix/",
	"chore/",
	"hotfix/",
	"release/",
}

// HumanizeBranchName converts a branch name into a human-readable title.
// It strips common prefixes (feature/, fix/, etc.), replaces dashes/underscores
// with spaces, and capitalizes the first word.
func HumanizeBranchName(branch string) string {
	name := branch
	for _, prefix := range commonBranchPrefixes {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}

	// Replace - and _ with spaces
	name = strings.NewReplacer("-", " ", "_", " ").Replace(name)

	// Trim spaces and capitalize first letter
	name = strings.TrimSpace(name)
	if name == "" {
		return branch
	}

	// Capitalize first character
	return strings.ToUpper(name[:1]) + name[1:]
}
