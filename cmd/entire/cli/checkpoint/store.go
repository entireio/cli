package checkpoint

import (
	"fmt"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// Compile-time check that GitStore implements the Store interface.
var _ Store = (*GitStore)(nil)

// GitStore provides operations for both temporary and committed checkpoint
// storage. Writes target refs.Primary; committed reads resolve against
// refs.Read.
type GitStore struct {
	repo        *git.Repository
	refs        CommittedRefs
	blobFetcher BlobFetchFunc
}

// NewGitStore creates a checkpoint store backed by the given git repository
// and committed-metadata topology. Pass DefaultV1Refs() for the v1-only default
// or ResolveCommittedRefs(ctx) in code paths that honor settings.
func NewGitStore(repo *git.Repository, refs CommittedRefs) *GitStore {
	return &GitStore{repo: repo, refs: refs}
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
func (s *GitStore) Refs() CommittedRefs {
	return s.refs
}

// CommittedReadRef returns the ref that committed-checkpoint reads resolve against.
func (s *GitStore) CommittedReadRef() plumbing.ReferenceName {
	return s.refs.Read
}

func (s *GitStore) setPrimaryRef(hash plumbing.Hash) error {
	if err := s.repo.Storer.SetReference(plumbing.NewHashReference(s.refs.Primary, hash)); err != nil {
		return fmt.Errorf("set primary metadata ref %s to %s: %w", s.refs.Primary, hash, err)
	}
	return nil
}
