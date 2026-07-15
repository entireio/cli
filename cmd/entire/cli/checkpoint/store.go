package checkpoint

import (
	"fmt"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

var (
	_ PersistentStore = (*GitStore)(nil)
	_ AuthorReader    = (*GitStore)(nil)
	_ Writer          = (*GitStore)(nil)
	_ EphemeralStore  = (*ephemeralStore)(nil)
)

// treeWriter holds the repo-only machinery for building a single checkpoint's
// subtree from write requests: entry builders, transcript/session writers, and
// the per-request appliers (applySessionWrite / applyTranscriptBackfill /
// applySummaryBackfill / applyAttributionBackfill). It is independent of where
// the resulting subtree is committed, so both the git-branch store (which nests
// the subtree under <shard>/<id>/ on the v1 branch) and the git-refs store
// (which keeps it at the root of a per-checkpoint ref) embed it and share this
// code.
type treeWriter struct {
	repo *git.Repository
}

// GitStore is the committed (persistent) checkpoint store. Writes target
// refs.Primary; committed reads resolve against refs.Read. The temporary
// shadow-branch surface lives in ephemeralStore. It embeds *treeWriter for the
// shared subtree-building machinery.
type GitStore struct {
	*treeWriter

	refs        PersistentRefs
	blobFetcher BlobFetchFunc
}

// ephemeralStore is the git shadow-branch (temporary) checkpoint store. It is
// an independent type from GitStore; the two share only package-level helpers.
type ephemeralStore struct {
	repo *git.Repository
	refs PersistentRefs
}

// newEphemeralStore creates the shadow-branch store for the given repository
// and committed-metadata topology (it consults refs.Primary to recognize the
// committed branch when listing shadow branches).
func newEphemeralStore(repo *git.Repository, refs PersistentRefs) *ephemeralStore {
	return &ephemeralStore{repo: repo, refs: refs}
}

// NewEphemeralStore constructs the git shadow-branch (temporary) checkpoint
// store. Most callers reach it via Open(...).Ephemeral(); this direct
// constructor exists for benchmarks and tests that exercise the shadow-branch
// surface without the full facade.
func NewEphemeralStore(repo *git.Repository, refs PersistentRefs) EphemeralStore {
	return newEphemeralStore(repo, refs)
}

// NewGitStore creates a checkpoint store backed by the given git repository
// and committed-metadata topology. Pass DefaultV1Refs() for the v1-only default
// or ResolveRefs(ctx) in code paths that honor settings.
func NewGitStore(repo *git.Repository, refs PersistentRefs) *GitStore {
	return &GitStore{treeWriter: &treeWriter{repo: repo}, refs: refs}
}

// SetBlobFetcher configures the store to automatically fetch missing blobs
// on demand when reading from metadata trees.
func (s *GitStore) SetBlobFetcher(f BlobFetchFunc) {
	s.blobFetcher = f
}

// Repository returns the underlying git repository.
func (s *GitStore) Repository() *git.Repository {
	return s.repo
}

// Refs returns the committed-metadata topology the store was constructed with.
func (s *GitStore) Refs() PersistentRefs {
	return s.refs
}

// PersistentReadRef returns the ref that committed-checkpoint reads resolve against.
func (s *GitStore) PersistentReadRef() plumbing.ReferenceName {
	return s.refs.Read
}

func (s *GitStore) setPrimaryRef(hash plumbing.Hash) error {
	if err := s.repo.Storer.SetReference(plumbing.NewHashReference(s.refs.Primary, hash)); err != nil {
		return fmt.Errorf("set primary metadata ref %s to %s: %w", s.refs.Primary, hash, err)
	}
	return nil
}
