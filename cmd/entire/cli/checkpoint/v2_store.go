package checkpoint

import (
	"fmt"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// V2GitStore provides checkpoint storage operations for the v2 ref layout.
// It writes to two custom refs under refs/entire/:
//   - /main: permanent metadata + compact transcripts
//   - /full/current: active generation of raw transcripts
//
// V2GitStore is separate from GitStore (v1) to keep concerns isolated
// and simplify future v1 removal. It composes GitStore internally to
// reuse ref-agnostic entry-building helpers (tree surgery, session
// indexing, summary aggregation).
type V2GitStore struct {
	repo *git.Repository
	gs   *GitStore // shared entry-building helpers (same package)

	// maxCheckpointsPerGeneration overrides the rotation threshold for testing.
	// Zero means use DefaultMaxCheckpointsPerGeneration.
	maxCheckpointsPerGeneration int
}

// maxCheckpoints returns the effective rotation threshold.
func (s *V2GitStore) maxCheckpoints() int {
	if s.maxCheckpointsPerGeneration > 0 {
		return s.maxCheckpointsPerGeneration
	}
	return DefaultMaxCheckpointsPerGeneration
}

// NewV2GitStore creates a new v2 checkpoint store backed by the given git repository.
func NewV2GitStore(repo *git.Repository) *V2GitStore {
	return &V2GitStore{
		repo: repo,
		gs:   &GitStore{repo: repo},
	}
}

// ensureRef ensures that a custom ref exists, creating an orphan commit
// with an empty tree if it does not.
func (s *V2GitStore) ensureRef(refName plumbing.ReferenceName) error {
	_, err := s.repo.Reference(refName, true)
	if err == nil {
		return nil // Already exists
	}

	emptyTreeHash, err := BuildTreeFromEntries(s.repo, make(map[string]object.TreeEntry))
	if err != nil {
		return fmt.Errorf("failed to build empty tree: %w", err)
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitHash, err := CreateCommit(s.repo, emptyTreeHash, plumbing.ZeroHash, "Initialize v2 ref", authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("failed to create initial commit: %w", err)
	}

	ref := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to set ref %s: %w", refName, err)
	}

	return nil
}

// getRefState returns the parent commit hash and root tree hash for a ref.
func (s *V2GitStore) getRefState(refName plumbing.ReferenceName) (parentHash, treeHash plumbing.Hash, err error) {
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("ref %s not found: %w", refName, err)
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get commit for ref %s: %w", refName, err)
	}

	return ref.Hash(), commit.TreeHash, nil
}

// updateRef creates a new commit on a ref with the given tree, updating the ref to point to it.
func (s *V2GitStore) updateRef(refName plumbing.ReferenceName, treeHash, parentHash plumbing.Hash, message, authorName, authorEmail string) error {
	commitHash, err := CreateCommit(s.repo, treeHash, parentHash, message, authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}

	ref := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to update ref %s: %w", refName, err)
	}

	return nil
}
