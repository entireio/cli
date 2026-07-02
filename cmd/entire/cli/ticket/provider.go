package ticket

import "context"

// Provider abstracts a ticket platform such as Linear or Jira. Implementations
// live in their own files and are wired into the command through the deps
// bridge in the cli package, keeping the command surface platform-agnostic.
//
// Providers are wired explicitly rather than self-registering via init, per the
// repository's gochecknoinits convention.
type Provider interface {
	// Name is the stable identifier used in settings, e.g. "linear".
	Name() string

	// Fetch resolves a provider-native ticket ID into a canonical Task.
	Fetch(ctx context.Context, id string) (Task, error)

	// Comment posts a comment on the ticket.
	Comment(ctx context.Context, id, body string) error

	// SetState transitions the ticket to the given normalized state. Providers
	// map State onto their own workflow states.
	SetState(ctx context.Context, id string, state State) error

	// ResolveFromBranch extracts a ticket ID from a git branch name when the
	// platform encodes one (e.g. Linear's "user/eng-142-title"). ok is false
	// when no ID can be inferred.
	ResolveFromBranch(branch string) (id string, ok bool)

	// CanonicalID normalizes a user-supplied identifier (which may be a full
	// ticket URL or a differently-cased id) into the provider's canonical form,
	// e.g. "MOH-57". ok is false when raw contains no recognizable identifier.
	// It performs no network I/O.
	CanonicalID(raw string) (id string, ok bool)
}
