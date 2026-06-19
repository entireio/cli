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
	Refs *CommittedRefs
}

// Stores is the facade returned by Open: the committed store plus the git-only
// temporary capability and resolved committed-ref topology.
type Stores struct {
	// Primary is the committed store — the source of truth that serves all
	// committed reads and writes.
	Primary *GitStore

	refs CommittedRefs
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
		Primary: store,
		refs:    refs,
	}, nil
}

func resolveOpenRefs(ctx context.Context, opts OpenOptions) CommittedRefs {
	if opts.Refs != nil {
		return *opts.Refs
	}
	return ResolveCommittedRefs(ctx)
}

// Temporary returns the git-backed temporary (shadow-branch) store. It is the
// same backing store as Primary; the name marks shadow-branch intent at the
// call site.
func (s *Stores) Temporary() *GitStore { return s.Primary }

// Refs returns the resolved committed-ref topology.
func (s *Stores) Refs() CommittedRefs { return s.refs }
