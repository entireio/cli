package checkpoint

import (
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Compile-time check that GitStore implements the Store interface.
var _ Store = (*GitStore)(nil)

// GitStore provides operations for both temporary and committed checkpoint storage.
// It implements the Store interface by wrapping a git repository.
type GitStore struct {
	repo        *git.Repository
	blobFetcher BlobFetchFunc
	// committedReadRef is the ref that committed-checkpoint *reads* resolve
	// against. Defaults to the v1 branch; v1.1 read stores bind it to the
	// local-only custom ref (paths.MetadataRefName).
	//
	// Writes intentionally do NOT use this ref: committed writes always target
	// the v1 branch (the durable source of truth) and are mirrored to the v1.1
	// custom ref separately by the strategy mirror paths. Pointing writes here
	// would let a v1.1 read store write ahead of v1 and diverge from it.
	committedReadRef plumbing.ReferenceName
}

// defaultCommittedReadRef is the v1 metadata branch ref reads resolve against by
// default.
func defaultCommittedReadRef() plumbing.ReferenceName {
	return plumbing.NewBranchReferenceName(paths.MetadataBranchName)
}

// NewGitStore creates a new checkpoint store backed by the given git repository.
// Committed reads resolve against the default v1 metadata branch.
func NewGitStore(repo *git.Repository) *GitStore {
	return &GitStore{repo: repo, committedReadRef: defaultCommittedReadRef()}
}

// NewGitStoreWithRef creates a checkpoint store whose committed reads resolve
// against committedReadRef (writes still target the v1 branch; see GitStore).
func NewGitStoreWithRef(repo *git.Repository, committedReadRef plumbing.ReferenceName) *GitStore {
	return &GitStore{repo: repo, committedReadRef: committedReadRef}
}

// SetBlobFetcher configures the store to automatically fetch missing blobs
// on demand when reading from metadata trees. This is used after treeless
// fetches where tree objects are local but blob objects are not.
func (s *GitStore) SetBlobFetcher(f BlobFetchFunc) {
	s.blobFetcher = f
}

// Repository returns the underlying git repository.
// This is useful for strategies that need direct repository access.
func (s *GitStore) Repository() *git.Repository {
	return s.repo
}

// CommittedReadRef returns the ref that committed-checkpoint reads resolve against.
func (s *GitStore) CommittedReadRef() plumbing.ReferenceName {
	return s.committedReadRef
}
