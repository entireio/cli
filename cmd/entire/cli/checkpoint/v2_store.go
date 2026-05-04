package checkpoint

import (
	"context"
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

	// FetchRemote is the git remote used for fetch-on-demand operations (e.g.,
	// fetching /full/* refs during entire resume). Defaults to "origin".
	// Set to the checkpoint remote URL when checkpoint_remote is configured.
	FetchRemote string

	// blobFetcher fetches missing blobs by hash. When set, read paths wrap
	// trees with FetchingTree so missing blobs are auto-recovered (and the
	// cat-file fallback covers partial-clone-filtered blobs that go-git's
	// storer can't see).
	blobFetcher BlobFetchFunc
}

// maxCheckpoints returns the effective rotation threshold.
func (s *V2GitStore) maxCheckpoints() int {
	if s.maxCheckpointsPerGeneration > 0 {
		return s.maxCheckpointsPerGeneration
	}
	return DefaultMaxCheckpointsPerGeneration
}

// NewV2GitStore creates a new v2 checkpoint store backed by the given git repository.
// fetchRemote is the git remote used for fetch-on-demand operations (e.g., fetching
// /full/* refs during entire resume). Pass "origin" or the checkpoint remote URL.
func NewV2GitStore(repo *git.Repository, fetchRemote string) *V2GitStore {
	if fetchRemote == "" {
		fetchRemote = "origin"
	}
	return &V2GitStore{
		repo:        repo,
		gs:          &GitStore{repo: repo},
		FetchRemote: fetchRemote,
	}
}

// SetBlobFetcher configures the store to automatically fetch missing blobs
// on demand when reading from /main trees. Mirrors GitStore.SetBlobFetcher.
// Required for reads against partial-clone repos where blobs may be absent
// or invisible to go-git's cached packfile index.
func (s *V2GitStore) SetBlobFetcher(f BlobFetchFunc) {
	s.blobFetcher = f
}

// wrapWithFetcher returns the input tree wrapped in a FetchingTree using
// the configured blob fetcher. Callers use the returned tree's File() /
// Tree() methods instead of the raw go-git ones so missing blobs are
// recovered via the fetcher and the cat-file fallback.
func (s *V2GitStore) wrapWithFetcher(ctx context.Context, tree *object.Tree) *FetchingTree {
	return NewFetchingTree(ctx, tree, s.repo.Storer, s.blobFetcher)
}

// ensureRef ensures that a custom ref exists, creating an orphan commit
// with an empty tree if it does not.
func (s *V2GitStore) ensureRef(ctx context.Context, refName plumbing.ReferenceName) error {
	_, err := s.repo.Reference(refName, true)
	if err == nil {
		return nil // Already exists
	}

	emptyTreeHash, err := BuildTreeFromEntries(ctx, s.repo, make(map[string]object.TreeEntry))
	if err != nil {
		return fmt.Errorf("failed to build empty tree: %w", err)
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitHash, err := CreateCommit(ctx, s.repo, emptyTreeHash, plumbing.ZeroHash, "Initialize v2 ref", authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("failed to create initial commit: %w", err)
	}

	ref := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to set ref %s: %w", refName, err)
	}

	return nil
}

// GetRefState returns the parent commit hash and root tree hash for a ref.
func (s *V2GitStore) GetRefState(refName plumbing.ReferenceName) (parentHash, treeHash plumbing.Hash, err error) {
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
func (s *V2GitStore) updateRef(ctx context.Context, refName plumbing.ReferenceName, treeHash, parentHash plumbing.Hash, message, authorName, authorEmail string) error {
	commitHash, err := CreateCommit(ctx, s.repo, treeHash, parentHash, message, authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}

	ref := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to update ref %s: %w", refName, err)
	}

	return nil
}
