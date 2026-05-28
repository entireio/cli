package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// V2GitStore provides read operations for the v2 ref layout.
// It reads from two custom refs under refs/entire/:
//   - /main: permanent metadata + compact transcripts
//   - /full/current: raw transcripts
//
// V2GitStore is separate from GitStore (v1) to keep concerns isolated
// and simplify future v1 removal.
type V2GitStore struct {
	repo *git.Repository
	gs   *GitStore // shared entry-building helpers (same package)

	// blobFetcher fetches missing blobs by hash. When set, read paths wrap
	// trees with FetchingTree so missing blobs are auto-recovered (and the
	// cat-file fallback covers partial-clone-filtered blobs that go-git's
	// storer can't see).
	blobFetcher BlobFetchFunc
}

// NewV2GitStore creates a new v2 checkpoint store backed by the given git repository.
func NewV2GitStore(repo *git.Repository) *V2GitStore {
	return &V2GitStore{
		repo: repo,
		gs:   &GitStore{repo: repo},
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

// GetRefState returns the parent commit hash and root tree hash for a ref.
// Falls back to `git rev-parse <hash>^{tree}` when go-git can't load the
// commit object — same partial-clone / stale-packfile-cache reason as
// readTreeEntriesViaCLI in parse_tree.go.
func (s *V2GitStore) GetRefState(refName plumbing.ReferenceName) (parentHash, treeHash plumbing.Hash, err error) {
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("ref %s not found: %w", refName, err)
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		cliTreeHash, cliErr := commitTreeHashViaCLI(context.Background(), ref.Hash())
		if cliErr != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get commit for ref %s: %w", refName, errors.Join(err, cliErr))
		}
		logging.Warn(context.Background(), "GetRefState: go-git commit read failed, used git rev-parse fallback",
			slog.String("ref", refName.String()),
			slog.String("commit", ref.Hash().String()[:12]),
			slog.String("gogit_error", err.Error()),
		)
		return ref.Hash(), cliTreeHash, nil
	}

	return ref.Hash(), commit.TreeHash, nil
}

// commitTreeHashViaCLI resolves the tree hash of a commit via
// `git rev-parse <hash>^{tree}`. See GetRefState for the rationale.
func commitTreeHashViaCLI(ctx context.Context, commitHash plumbing.Hash) (plumbing.Hash, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", commitHash.String()+"^{tree}")
	output, err := cmd.Output()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git rev-parse %s^{tree}: %w", commitHash.String()[:12], err)
	}
	hex := strings.TrimSpace(string(output))
	if hex == "" {
		return plumbing.ZeroHash, fmt.Errorf("git rev-parse %s^{tree}: empty output", commitHash.String()[:12])
	}
	return plumbing.NewHash(hex), nil
}
