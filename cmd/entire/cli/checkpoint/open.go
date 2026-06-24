package checkpoint

import (
	"context"

	"github.com/go-git/go-git/v6"
)

// OpenOptions configures Open. The zero value uses the default committed-ref
// topology and attaches no blob fetcher.
type OpenOptions struct {
	// BlobFetcher is the CLI-level on-demand blob fetcher. The checkpoint
	// package cannot resolve it itself, so the CLI layer injects it here and
	// Open attaches it to the constructed store(s). nil leaves on-demand
	// fetching off.
	BlobFetcher BlobFetchFunc

	// Refs overrides the default committed-ref topology. A non-nil value wins,
	// e.g. attach pins reads to Primary via PrimaryAsRead().
	Refs *PersistentRefs
}

// Stores is the facade returned by Open: the persistent store plus the git-only
// ephemeral (shadow-branch) capability and resolved committed-ref topology.
type Stores struct {
	// Persistent is the committed store that serves permanent reads and writes.
	Persistent PersistentStore

	ephemeral EphemeralStore
	refs      PersistentRefs
}

// Open resolves the checkpoint storage topology and constructs the backing
// store. It keeps ref resolution and blob-fetcher wiring in one place.
//
//nolint:unparam // Callers treat store construction as fallible at this boundary; the git backend has no fallible setup today.
func Open(ctx context.Context, repo *git.Repository, opts OpenOptions) (*Stores, error) {
	refs := resolveOpenRefs(ctx, opts)
	store := NewGitStore(repo, refs)
	if opts.BlobFetcher != nil {
		store.SetBlobFetcher(opts.BlobFetcher)
	}
	return &Stores{
		Persistent: store,
		ephemeral:  newEphemeralStore(repo, refs),
		refs:       refs,
	}, nil
}

func resolveOpenRefs(ctx context.Context, opts OpenOptions) PersistentRefs {
	if opts.Refs != nil {
		return *opts.Refs
	}
	return ResolveRefs(ctx)
}

// Ephemeral returns the git-backed shadow-branch (temporary) store.
func (s *Stores) Ephemeral() EphemeralStore { return s.ephemeral }

// Refs returns the resolved committed-ref topology.
func (s *Stores) Refs() PersistentRefs { return s.refs }
