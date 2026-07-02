package ticket

// State is a normalized ticket lifecycle state. Providers map their
// platform-specific workflow states onto these values so callers stay
// platform-agnostic.
type State string

const (
	// StateUnknown is the zero value, used when a state cannot be mapped.
	StateUnknown State = ""
	// StateTodo is an unstarted ticket.
	StateTodo State = "todo"
	// StateInProgress is a ticket actively being worked.
	StateInProgress State = "in_progress"
	// StateInReview is a ticket whose work is awaiting review.
	StateInReview State = "in_review"
	// StateDone is a completed ticket.
	StateDone State = "done"
)

// Comment is a single comment on a ticket.
type Comment struct {
	Author string
	Body   string
}

// Task is the platform-agnostic representation of a ticket. Providers map their
// native issue/story/work-item shape into this canonical form so the rest of
// the CLI never depends on a specific tracker.
type Task struct {
	// ID is the provider-native identifier, e.g. "ENG-142".
	ID string
	// Title is the one-line summary.
	Title string
	// Intent is the ticket body/description — the "what was asked" that grounds
	// agent work and review.
	Intent string
	// Acceptance holds acceptance criteria when the provider exposes them
	// separately from the body; empty otherwise.
	Acceptance string
	// State is the normalized lifecycle state.
	State State
	// URL is the canonical web URL of the ticket.
	URL string
	// BranchName is the platform's suggested git branch name for this ticket
	// (e.g. Linear's issue.branchName); empty when the platform has none.
	BranchName string
	// Labels are the ticket's labels or tags.
	Labels []string
	// Comments are the ticket's comments, oldest first.
	Comments []Comment
}
