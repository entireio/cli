package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

var (
	_ PersistentStore = (*gitRefsStore)(nil)
	_ AuthorReader    = (*gitRefsStore)(nil)
	_ Writer          = (*gitRefsStore)(nil)
)

// gitRefsStore is the git-backed persistent checkpoint store that keeps one ref
// per checkpoint at refs/entire/checkpoints/<shard>/<id>. Each ref points at a
// commit whose tree root IS that checkpoint's contents (metadata.json, 0/, 1/,
// tasks/…), so updates advance the ref and preserve per-checkpoint history. It
// shares the checkpoint-subtree machinery with the git-branch store via the
// embedded *treeWriter (anchored at basePath ""), differing only in where the
// subtree is committed: a per-checkpoint ref instead of the v1 branch.
type gitRefsStore struct {
	*treeWriter

	blobFetcher BlobFetchFunc
	refFetcher  RefFetchFunc
}

// newGitRefsStore constructs the per-checkpoint-ref store for a repository.
func newGitRefsStore(repo *git.Repository) *gitRefsStore {
	return &gitRefsStore{treeWriter: &treeWriter{repo: repo}}
}

// SetBlobFetcher configures on-demand blob fetching for reads from ref trees.
func (s *gitRefsStore) SetBlobFetcher(f BlobFetchFunc) {
	s.blobFetcher = f
}

// SetRefFetcher configures on-demand fetching of a missing checkpoint ref (e.g.
// a checkpoint written on another machine). nil leaves reads local-only.
func (s *gitRefsStore) SetRefFetcher(f RefFetchFunc) {
	s.refFetcher = f
}

// Write dispatches a persistent write request to the matching ref operation,
// mirroring the git-branch store's Write.
func (s *gitRefsStore) Write(ctx context.Context, req WriteRequest) error {
	switch r := req.(type) {
	case Session:
		return s.writeSession(ctx, WriteOptions(r))
	case SessionTranscript:
		return s.backfillTranscript(ctx, UpdateOptions(r))
	case SessionSummary:
		return s.backfillSummary(ctx, r.CheckpointID, r.Summary)
	case CheckpointAttribution:
		return s.backfillAttribution(ctx, r.CheckpointID, r.Attribution)
	default:
		return fmt.Errorf("checkpoint: unsupported write request %T", req)
	}
}

// refBase resolves a checkpoint ref's current tip commit (the parent for the
// next write) and subtree object (the checkpoint's current contents). A missing
// ref yields (ZeroHash, nil) so the next write becomes an orphan commit.
func (s *gitRefsStore) refBase(cid id.CheckpointID) (plumbing.Hash, *object.Tree, error) {
	refName, err := RefName(cid)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	ref, err := s.repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil, nil // no ref yet → new checkpoint (orphan)
	}
	if err != nil {
		// A real lookup failure (IO/corruption), not an absent ref: surface it
		// rather than silently starting a fresh orphan history over the ref.
		return plumbing.ZeroHash, nil, fmt.Errorf("resolve checkpoint ref %s: %w", refName, err)
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("read checkpoint commit %s: %w", ref.Hash(), err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("read checkpoint tree for %s: %w", cid, err)
	}
	return ref.Hash(), tree, nil
}

// setRef points a checkpoint's ref at a new commit and records it for push.
// Enqueue is best-effort: a write that lands locally but fails to enqueue must
// not fail condensation. The ref is still local; only its remote sync is missed
// until a later write to the same checkpoint re-enqueues it.
func (s *gitRefsStore) setRef(ctx context.Context, cid id.CheckpointID, hash plumbing.Hash) error {
	refName, err := RefName(cid)
	if err != nil {
		return err
	}
	if err := s.repo.Storer.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
		return fmt.Errorf("set checkpoint ref %s to %s: %w", refName, hash, err)
	}
	s.enqueueForPush(ctx, refName)
	return nil
}

// enqueueForPush records refName in the push-discovery queue, logging (never
// returning) on failure so the local ref write still succeeds.
func (s *gitRefsStore) enqueueForPush(ctx context.Context, refName plumbing.ReferenceName) {
	q, err := PushQueueForRepo(ctx, s.repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint: resolve push queue failed; ref not enqueued",
			slog.String("ref", refName.String()), slog.String("error", err.Error()))
		return
	}
	if err := q.Enqueue(refName); err != nil {
		logging.Warn(ctx, "checkpoint: enqueue checkpoint ref for push failed",
			slog.String("ref", refName.String()), slog.String("error", err.Error()))
	}
}

func (s *gitRefsStore) writeSession(ctx context.Context, opts WriteOptions) error {
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid checkpoint options: checkpoint ID is required")
	}
	if err := validation.ValidateSessionID(opts.SessionID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}
	if err := validation.ValidateToolUseID(opts.ToolUseID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}
	if err := validation.ValidateAgentID(opts.AgentID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}

	parentHash, existing, err := s.refBase(opts.CheckpointID)
	if err != nil {
		return err
	}

	checkpointSubtree, taskMetadataPath, err := s.applySessionWrite(ctx, opts, existing, "")
	if err != nil {
		return err
	}

	commitMsg := s.buildCommitMessage(opts, taskMetadataPath)
	commitHash, err := CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, opts.AuthorName, opts.AuthorEmail)
	if err != nil {
		return err
	}
	return s.setRef(ctx, opts.CheckpointID, commitHash)
}

func (s *gitRefsStore) backfillTranscript(ctx context.Context, opts UpdateOptions) error {
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid update options: checkpoint ID is required")
	}

	parentHash, existing, err := s.refBase(opts.CheckpointID)
	if err != nil {
		return err
	}

	// applyTranscriptBackfill returns ErrCheckpointNotFound when the ref has no
	// root summary yet (existing == nil → empty entries), matching the git-branch
	// store's behavior for backfilling an unknown checkpoint.
	checkpointSubtree, err := s.applyTranscriptBackfill(ctx, opts, existing, "")
	if err != nil {
		return err
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Finalize transcript for Checkpoint: %s", opts.CheckpointID)
	commitHash, err := CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, authorName, authorEmail)
	if err != nil {
		return err
	}
	return s.setRef(ctx, opts.CheckpointID, commitHash)
}

func (s *gitRefsStore) backfillSummary(ctx context.Context, checkpointID id.CheckpointID, summary *Summary) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}

	parentHash, existing, err := s.refBase(checkpointID)
	if err != nil {
		return err
	}

	checkpointSubtree, sessionID, err := s.applySummaryBackfill(ctx, existing, "", summary)
	if err != nil {
		return err
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Update summary for checkpoint %s (session: %s)", checkpointID, sessionID)
	commitHash, err := CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, authorName, authorEmail)
	if err != nil {
		return err
	}
	return s.setRef(ctx, checkpointID, commitHash)
}

func (s *gitRefsStore) backfillAttribution(ctx context.Context, checkpointID id.CheckpointID, combinedAttribution *Attribution) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}

	parentHash, existing, err := s.refBase(checkpointID)
	if err != nil {
		return err
	}

	checkpointSubtree, err := s.applyAttributionBackfill(ctx, existing, "", combinedAttribution)
	if err != nil {
		return err
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Update checkpoint summary for %s", checkpointID)
	commitHash, err := CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, authorName, authorEmail)
	if err != nil {
		return err
	}
	return s.setRef(ctx, checkpointID, commitHash)
}

// checkpointTree resolves a FetchingTree rooted at a checkpoint's ref commit
// tree (which is the checkpoint subtree itself). Returns ErrCheckpointNotFound
// when the ref or its commit/tree cannot be resolved.
func (s *gitRefsStore) checkpointTree(ctx context.Context, cid id.CheckpointID) (*FetchingTree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	ref, err := s.resolveRefMaybeFetch(ctx, cid)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, ErrCheckpointNotFound
		}
		return nil, err
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		// The ref resolved but its commit object doesn't — corruption/IO, not an
		// absent checkpoint. Surface it instead of masking as "not found".
		return nil, fmt.Errorf("read checkpoint commit %s for %s: %w", ref.Hash(), cid, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read checkpoint tree for %s: %w", cid, err)
	}
	return NewFetchingTree(ctx, tree, s.repo.Storer, s.blobFetcher), nil
}

// resolveRefMaybeFetch resolves a checkpoint ref, fetching it from the remote
// once when it is missing locally and a ref fetcher is configured (the
// checkpoint may have been written on another machine). It distinguishes a
// genuinely absent ref (returns a plumbing.ErrReferenceNotFound-wrapped error,
// which callers map to ErrCheckpointNotFound) from a real failure — an IO error,
// or a fetch that failed for network/context reasons — which is returned as-is
// so it is not silently swallowed as "checkpoint not found".
func (s *gitRefsStore) resolveRefMaybeFetch(ctx context.Context, cid id.CheckpointID) (*plumbing.Reference, error) {
	refName, err := RefName(cid)
	if err != nil {
		return nil, err
	}
	ref, err := s.repo.Reference(refName, true)
	if err == nil {
		return ref, nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, fmt.Errorf("resolve checkpoint ref %s: %w", refName, err)
	}
	if s.refFetcher == nil {
		return nil, err //nolint:wrapcheck // genuinely absent; caller maps ErrReferenceNotFound to ErrCheckpointNotFound
	}
	if fetchErr := s.refFetcher(ctx, refName); fetchErr != nil {
		logging.Debug(ctx, "git-refs: on-demand checkpoint ref fetch failed",
			slog.String("ref", refName.String()), slog.String("error", fetchErr.Error()))
		return nil, fmt.Errorf("fetch checkpoint ref %s: %w", refName, fetchErr)
	}
	// Re-resolve after a successful fetch. ErrReferenceNotFound here means the
	// remote genuinely has no such checkpoint; anything else is a real error.
	ref, err = s.repo.Reference(refName, true)
	if err != nil {
		return nil, err //nolint:wrapcheck // ErrReferenceNotFound (absent) or a real error; caller distinguishes via errors.Is
	}
	return ref, nil
}

// sessionTree resolves the FetchingTree for one session within a checkpoint ref.
func (s *gitRefsStore) sessionTree(ctx context.Context, cid id.CheckpointID, sessionIndex int) (*FetchingTree, error) {
	ct, err := s.checkpointTree(ctx, cid)
	if err != nil {
		return nil, err
	}
	sessionTree, err := ct.Tree(strconv.Itoa(sessionIndex))
	if err != nil {
		return nil, fmt.Errorf("%w: session %d not found: %w", ErrCheckpointNotFound, sessionIndex, err)
	}
	return sessionTree, nil
}

// Read returns the checkpoint summary, or (nil, nil) when the checkpoint's ref
// is absent, so the contract normalizes it to ErrCheckpointNotFound.
func (s *gitRefsStore) Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	ct, err := s.checkpointTree(ctx, checkpointID)
	if err != nil {
		if errors.Is(err, ErrCheckpointNotFound) {
			return nil, nil //nolint:nilnil // absent ref → no checkpoint; contract normalizes to ErrCheckpointNotFound
		}
		return nil, err
	}
	return readSummaryFromCheckpointTree(ct)
}

func (s *gitRefsStore) ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, error) {
	sessionTree, err := s.sessionTree(ctx, checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}
	return readSessionMetadataFromTree(sessionTree, sessionIndex)
}

func (s *gitRefsStore) ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, string, error) {
	sessionTree, err := s.sessionTree(ctx, checkpointID, sessionIndex)
	if err != nil {
		return nil, "", err
	}
	return readSessionMetadataAndPromptsFromTree(sessionTree, sessionIndex)
}

func (s *gitRefsStore) ReadSessionPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error) {
	sessionTree, err := s.sessionTree(ctx, checkpointID, sessionIndex)
	if err != nil {
		return "", err
	}
	return readSessionPromptsFromTree(sessionTree)
}

func (s *gitRefsStore) ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	sessionTree, err := s.sessionTree(ctx, checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}
	return readSessionContentFromTree(ctx, sessionTree)
}

// List enumerates local checkpoint refs and reads each root summary, sorted most
// recent first. Storage-level listing is local-refs-only for now (no remote
// enumeration), matching the issue's first-version scope.
func (s *gitRefsStore) List(ctx context.Context) ([]CheckpointInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	refs, err := s.repo.References()
	if err != nil {
		return nil, fmt.Errorf("list checkpoint refs: %w", err)
	}
	defer refs.Close()

	var checkpoints []CheckpointInfo
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		cid, ok := ParseRef(ref.Name())
		if !ok {
			return nil
		}
		commit, commitErr := s.repo.CommitObject(ref.Hash())
		if commitErr != nil {
			return nil //nolint:nilerr // skip unreadable refs, keep listing
		}
		tree, treeErr := commit.Tree()
		if treeErr != nil {
			return nil //nolint:nilerr // skip unreadable refs, keep listing
		}
		checkpoints = append(checkpoints, readCommittedInfoFromCheckpointTree(cid, tree))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate checkpoint refs: %w", err)
	}

	sortCheckpointInfosByRecency(checkpoints)
	return checkpoints, nil
}

// GetCheckpointAuthor returns the author of the checkpoint ref's tip commit (the
// most recent writer). Returns a zero Author when the ref is absent.
func (s *gitRefsStore) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error) {
	if err := ctx.Err(); err != nil {
		return Author{}, err //nolint:wrapcheck // Propagating context cancellation
	}
	refName, err := RefName(checkpointID)
	if err != nil {
		return Author{}, nil //nolint:nilerr // invalid ID → unknown author
	}
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return Author{}, nil //nolint:nilerr // no ref → unknown author
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return Author{}, nil //nolint:nilerr // unreadable → unknown author
	}
	return Author{Name: commit.Author.Name, Email: commit.Author.Email}, nil
}
