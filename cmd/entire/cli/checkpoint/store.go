package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
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

	refs                  PersistentRefs
	blobFetcher           BlobFetchFunc
	metadataBranchFetcher MetadataBranchFetchFunc
	// metadataBranchFetchTried latches the one recovery attempt per store; see
	// tryFetchMetadataBranch.
	metadataBranchFetchTried bool
	// readRemotes is the ordered checkpoint read-candidate chain consulted by
	// committed reads after the local tree; see OpenOptions.ReadRemotes. nil
	// means the legacy origin-only fallback.
	readRemotes []string
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

// SetReadRemotes configures the ordered checkpoint read-candidate remotes
// (elected sync remote first, then the legacy origin tier) whose tracking
// refs committed reads consult after the local tree. Selection happens by
// requested checkpoint, so an existing but incomplete tree does not mask a
// later candidate. This is a pure read — the chain never seeds or advances
// local refs. nil keeps the legacy origin-only fallback.
func (s *GitStore) SetReadRemotes(remotes []string) {
	s.readRemotes = remotes
}

// SetMetadataBranchFetcher configures the store to fetch the metadata branch
// from the checkpoint remote when it is missing both locally and on origin.
// See MetadataBranchFetchFunc for when it is appropriate to wire this.
func (s *GitStore) SetMetadataBranchFetcher(f MetadataBranchFetchFunc) {
	s.metadataBranchFetcher = f
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

// casPrimaryRef advances the primary metadata ref only if it still holds
// expectedOld, so a writer that read the tip, built a commit on it, and lost a
// race in between cannot force its commit over the winner's and orphan it.
//
// A lost compare-and-swap surfaces as storage.ErrReferenceHasChanged (wrapped),
// which is the signal to re-read the tip and rebuild — see
// writeWithRefRaceRetry. Every other failure is a genuine storage error.
func (s *GitStore) casPrimaryRef(expectedOld, hash plumbing.Hash) error {
	err := s.repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(s.refs.Primary, hash),
		plumbing.NewHashReference(s.refs.Primary, expectedOld),
	)
	if err != nil {
		return fmt.Errorf("compare-and-set primary metadata ref %s to %s: %w", s.refs.Primary, hash, err)
	}
	return nil
}

// refRaceRetryAttempts bounds how many times a compare-and-swap writer rebuilds
// its commit on a freshly-read tip. Five is far past what contention produces in
// practice — the racers are one detached backfill per session of a single
// commit, each holding the tip for one small tree write — and the bound exists
// so a pathologically busy ref drops the write instead of spinning forever.
const refRaceRetryAttempts = 5

// writeWithRefRaceRetry runs write until it succeeds, fails for a reason other
// than a lost compare-and-swap, or has rebuilt refRaceRetryAttempts times.
//
// write must re-read the ref it targets on every call: the whole point is that
// the commit is rebuilt on whatever tip the winner left behind, so the loser's
// document lands ON TOP of the winner's rather than replacing it.
//
// Used by writers on the shared v1 metadata ref — session writes, backfills,
// and the entity-deltas child — which hold no lock in common, so the tip
// genuinely can move under any of them.
func writeWithRefRaceRetry(ctx context.Context, what string, write func() error) error {
	var err error
	for attempt := 1; attempt <= refRaceRetryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = write()
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrReferenceHasChanged) {
			return err
		}
		logging.Debug(ctx, "checkpoint: ref moved under a write; rebuilding on the new tip",
			slog.String("write", what),
			slog.Int("attempt", attempt),
		)
	}
	logging.Warn(ctx, "checkpoint: giving up on a write that kept losing the ref race",
		slog.String("write", what),
		slog.Int("attempts", refRaceRetryAttempts),
		slog.String("error", err.Error()),
	)
	return err
}
