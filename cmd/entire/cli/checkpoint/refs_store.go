package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// ListHydrationTimeout is the per-ref budget for hydrating names-only List stubs
// during user-facing enumeration (after the display-limit truncate). Shorter than
// the default on-demand fetch budget so a stuck remote cannot turn list/explain
// into many minutes of sequential ref fetches.
const ListHydrationTimeout = 15 * time.Second

// ListHydrationPassTimeout bounds the entire List/explain stub-hydration pass.
// Without this, a slow remote can burn stub_count * ListHydrationTimeout
// (limit defaults to 100 and is user-settable via --limit).
const ListHydrationPassTimeout = 30 * time.Second

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

	blobFetcher     BlobFetchFunc
	refFetcher      RefFetchFunc
	remoteRefLister RemoteRefListFunc

	// fetchFailureMu guards fetchFailure: the first transport-level ref-fetch
	// failure, memoized for the store's lifetime so a loop over N missing refs
	// (e.g. a stop hook finalizing every checkpoint of a turn) pays a dead —
	// or too-slow-for-the-budget — network once instead of N times. Genuine
	// remote absence is per-ref and
	// is never memoized. The memo never clears — safe because every
	// fetcher-wired store today is opened per command/hook invocation; a
	// long-lived fetcher-wired store would need an expiry before reusing this.
	fetchFailureMu sync.Mutex
	fetchFailure   error
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

// SetRemoteRefLister configures remote checkpoint-ref enumeration for List (see
// RemoteRefListFunc). It only takes effect when List is called on a context
// marked by WithRemoteListDiscovery, so the per-turn hook hot path — which
// lists local refs without opting in — never triggers a network round trip. nil
// leaves List local-only.
func (s *gitRefsStore) SetRemoteRefLister(f RemoteRefListFunc) {
	s.remoteRefLister = f
}

// remoteListDiscoveryKey marks a context as permitting List to enumerate the
// checkpoint remote. It is an unexported key type so only this package can set
// or read the marker.
type remoteListDiscoveryKey struct{}

// WithRemoteListDiscovery marks ctx to allow gitRefsStore.List to enumerate
// checkpoint refs on the configured checkpoint remote (see RemoteRefListFunc)
// and surface not-yet-local checkpoints. Set it only on explicit, user-facing
// enumeration flows (e.g. `entire checkpoint list` / the branch `explain`
// view), never on the per-turn commit hook: routine local listings must stay
// network-free. Without this marker List is local-only regardless of whether a
// remote lister is configured.
func WithRemoteListDiscovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, remoteListDiscoveryKey{}, true)
}

// remoteListDiscoveryEnabled reports whether ctx was marked via
// WithRemoteListDiscovery.
func remoteListDiscoveryEnabled(ctx context.Context) bool {
	v, ok := ctx.Value(remoteListDiscoveryKey{}).(bool)
	return ok && v
}

// Write dispatches a persistent write request to the matching ref operation,
// mirroring the git-branch store's Write.
func (s *gitRefsStore) Write(ctx context.Context, req WriteRequest) error {
	switch r := req.(type) {
	case Session:
		return s.writeSession(ctx, WriteOptions(r))
	case ReservedSession:
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
// next write) and subtree object (the checkpoint's current contents) with a
// LOCAL-ONLY lookup. A missing ref yields (ZeroHash, nil) so the next write
// becomes an orphan commit — correct for creates, whose ref never exists yet
// (locally or remotely); probing the remote would add a doomed round-trip to
// every condensation and, with a fetcher configured, fail offline writes.
// Backfills, which target an existing checkpoint, use refBaseForBackfill
// instead. Migration (migrate.go) also uses refBase deliberately: it imports
// from the LOCAL v1 branch and must never probe the remote, even though its
// target ref may already exist. One writeSession caller does target an
// existing checkpoint — attach, which adds a session to it — but attach
// pre-fetches and verifies the ref's presence itself (refreshCheckpoint)
// before writing, so the local-only probe is safe there too.
func (s *gitRefsStore) refBase(cid id.CheckpointID) (plumbing.Hash, *object.Tree, error) {
	ref, err := s.resolveLocalRef(cid)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil, nil // no ref yet → new checkpoint (orphan)
	}
	if err != nil {
		// A real lookup failure (IO/corruption), not an absent ref: surface it
		// rather than silently starting a fresh orphan history over the ref.
		return plumbing.ZeroHash, nil, err
	}
	return s.refTip(cid, ref)
}

// resolveLocalRef resolves cid's checkpoint ref from LOCAL refs alone, trying
// the canonical RefName spelling first and then the case-folded one.
//
// The fallback is not defensive tidying. On a case-insensitive-but-case-
// preserving filesystem a ref is written THROUGH the canonical name but STORED
// under whichever spelling already named its shard bucket, and the two agree
// only while the ref is loose, because there the filesystem resolves the
// canonical name onto the folded directory for us. git pack-refs — which git gc
// --auto runs on its own — then moves the ref into packed-refs under its
// stored, folded name, where lookup is an exact string match and the canonical
// name stops resolving. Without the fallback the checkpoint reads as absent:
// Read reports ErrCheckpointNotFound for an intact checkpoint, and refBase
// hands the writer a ZeroHash parent, restarting the checkpoint's history as an
// orphan under a second ref. See FoldedRefName for why exactly one extra
// lookup is exhaustive rather than a heuristic.
//
// It reports plumbing.ErrReferenceNotFound only when NEITHER spelling resolves,
// so callers keep distinguishing absence from an IO failure.
func (s *gitRefsStore) resolveLocalRef(cid id.CheckpointID) (*plumbing.Reference, error) {
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
	folded, ok := FoldedRefName(cid)
	if !ok {
		return nil, err //nolint:wrapcheck // absent; caller distinguishes via errors.Is
	}
	foldedRef, foldedErr := s.repo.Reference(folded, true)
	if foldedErr == nil {
		return foldedRef, nil
	}
	if !errors.Is(foldedErr, plumbing.ErrReferenceNotFound) {
		return nil, fmt.Errorf("resolve checkpoint ref %s: %w", folded, foldedErr)
	}
	// Absent under both spellings: report the canonical miss, so the error
	// names the ref the caller asked for.
	return nil, err //nolint:wrapcheck // absent; caller distinguishes via errors.Is
}

// refBaseForBackfill resolves like refBase, but a ref missing locally is
// first fetched once from the remote (resolveRefMaybeFetch) when a fetcher is
// configured: a backfill targets an EXISTING checkpoint that may have been
// written or migrated on another machine, and declaring it absent without
// looking remotely diverges from the read path — the backfill would be
// handled as targeting a nonexistent checkpoint while reads, which DO fetch,
// serve the refs copy, leaving the backfilled data permanently invisible.
// A ref absent even after the fetch yields (ZeroHash, nil), which the
// backfill helpers report as ErrCheckpointNotFound — the signal that the
// checkpoint does not exist in this backend. A fetch FAILURE is returned
// as-is: transient unavailability must never masquerade as absence, because
// a caller or routing layer acting on a false "absent" would misdirect the
// backfill (e.g. onto a stale copy in another backend) instead of retrying.
func (s *gitRefsStore) refBaseForBackfill(ctx context.Context, cid id.CheckpointID) (plumbing.Hash, *object.Tree, error) {
	ref, err := s.resolveRefMaybeFetch(ctx, cid)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil, nil // genuinely absent → backfill reports not-found
	}
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	return s.refTip(cid, ref)
}

// refTip reads the commit and tree at a resolved checkpoint ref.
func (s *gitRefsStore) refTip(cid id.CheckpointID, ref *plumbing.Reference) (plumbing.Hash, *object.Tree, error) {
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

// enqueueForPush records refName in the push-discovery queue, logging (never
// returning) on failure so the local ref write still succeeds.
//
// The queue is resolved with the cancellation stripped from ctx. By the time we
// get here the ref is already written locally, and the queue is the ONLY
// push-discovery mechanism there is — see the
// pushQueueFileName doc. A ref that misses the queue is never pushed, and the
// writers above are idempotent, so a re-run skips it as already-present and
// never re-enqueues it: it stays local-only forever. Queue resolution is now a
// context-free metadata read, but stripping cancellation here keeps the whole
// post-ref bookkeeping boundary explicit and prevents a future queue operation
// from dropping a write that already happened.
func (s *gitRefsStore) enqueueForPush(ctx context.Context, refName plumbing.ReferenceName) {
	q, err := PushQueueForRepo(context.WithoutCancel(ctx), s.repo)
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

// writeRefName is the ref spelling a write must target: the one an existing ref
// already uses — which may be the case-folded shard, see resolveLocalRef — and
// the canonical RefName for a checkpoint that has none.
//
// Writing through the canonical name unconditionally is what forks a folded
// checkpoint in two once packing has taken away the filesystem's case-
// insensitive resolution: refBase finds the old ref and hands back its tip,
// while the CAS update creates a SECOND ref at the canonical spelling. The
// compare-and-swap cannot succeed there either, since git resolves the expected
// old value under the name being updated and that name holds nothing.
//
// A resolution failure falls back to the canonical name rather than failing the
// write: the checkpoint write is the operation that matters, and a real IO
// problem resurfaces immediately in updatePersistentRef's CAS.
//
// The push queue is enqueued with the name this returns, which is deliberate:
// it is the spelling that actually resolves locally, so the refspec works, and
// another clone reads whichever spelling arrives back through the same fallback.
func (s *gitRefsStore) writeRefName(cid id.CheckpointID) (plumbing.ReferenceName, error) {
	canonical, err := RefName(cid)
	if err != nil {
		return "", err
	}
	ref, err := s.resolveLocalRef(cid)
	if err != nil {
		return canonical, nil //nolint:nilerr // absent or unreadable → write the canonical name
	}
	return ref.Name(), nil
}

func (s *gitRefsStore) updateCheckpointRef(
	ctx context.Context,
	cid id.CheckpointID,
	base func() (plumbing.Hash, *object.Tree, error),
	build func(parentHash plumbing.Hash, existing *object.Tree) (plumbing.Hash, error),
) error {
	refName, err := s.writeRefName(cid)
	if err != nil {
		return err
	}
	err = updatePersistentRef(ctx, s.repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		parentHash, existing, baseErr := base()
		if baseErr != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, baseErr
		}
		newHash, buildErr := build(parentHash, existing)
		return newHash, parentHash, buildErr
	})
	if err != nil {
		return err
	}
	s.enqueueForPush(ctx, refName)
	return nil
}

func (s *gitRefsStore) writeSession(ctx context.Context, opts WriteOptions) error {
	// Parity with the backfill writers above and with GitStore.writeSession: a
	// canceled ctx means stop doing work, and creating a checkpoint is the most
	// expensive write there is (tree building plus a commit). Without this a
	// bulk writer that ignores cancellation — `entire import` was one — keeps
	// minting checkpoints after Ctrl-C.
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid checkpoint options: checkpoint ID is required")
	}
	if err := validation.ValidateSessionID(opts.SessionID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}
	return s.updateCheckpointRef(ctx, opts.CheckpointID,
		func() (plumbing.Hash, *object.Tree, error) {
			return s.refBase(opts.CheckpointID)
		},
		func(parentHash plumbing.Hash, existing *object.Tree) (plumbing.Hash, error) {
			checkpointSubtree, err := s.applySessionWrite(ctx, opts, existing, "")
			if err != nil {
				return plumbing.ZeroHash, err
			}
			commitMsg := s.buildCommitMessage(opts)
			return CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, opts.AuthorName, opts.AuthorEmail)
		},
	)
}

func (s *gitRefsStore) backfillTranscript(ctx context.Context, opts UpdateOptions) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid update options: checkpoint ID is required")
	}

	return s.updateCheckpointRef(ctx, opts.CheckpointID,
		func() (plumbing.Hash, *object.Tree, error) {
			return s.refBaseForBackfill(ctx, opts.CheckpointID)
		},
		func(parentHash plumbing.Hash, existing *object.Tree) (plumbing.Hash, error) {
			checkpointSubtree, err := s.applyTranscriptBackfill(ctx, opts, existing, "")
			if err != nil {
				return plumbing.ZeroHash, err
			}
			authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
			commitMsg := fmt.Sprintf("Finalize transcript for Checkpoint: %s", opts.CheckpointID)
			return CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, authorName, authorEmail)
		},
	)
}

func (s *gitRefsStore) backfillSummary(ctx context.Context, checkpointID id.CheckpointID, summary *Summary) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}

	return s.updateCheckpointRef(ctx, checkpointID,
		func() (plumbing.Hash, *object.Tree, error) {
			return s.refBaseForBackfill(ctx, checkpointID)
		},
		func(parentHash plumbing.Hash, existing *object.Tree) (plumbing.Hash, error) {
			checkpointSubtree, sessionID, err := s.applySummaryBackfill(ctx, existing, "", summary)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
			commitMsg := fmt.Sprintf("Update summary for checkpoint %s (session: %s)", checkpointID, sessionID)
			return CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, authorName, authorEmail)
		},
	)
}

func (s *gitRefsStore) backfillAttribution(ctx context.Context, checkpointID id.CheckpointID, combinedAttribution *Attribution) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}

	return s.updateCheckpointRef(ctx, checkpointID,
		func() (plumbing.Hash, *object.Tree, error) {
			return s.refBaseForBackfill(ctx, checkpointID)
		},
		func(parentHash plumbing.Hash, existing *object.Tree) (plumbing.Hash, error) {
			checkpointSubtree, err := s.applyAttributionBackfill(ctx, existing, "", combinedAttribution)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
			commitMsg := fmt.Sprintf("Update checkpoint summary for %s", checkpointID)
			return CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, commitMsg, authorName, authorEmail)
		},
	)
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
	ref, err := s.resolveLocalRef(cid)
	if err == nil {
		return ref, nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, err
	}
	if s.refFetcher == nil {
		return nil, err
	}
	s.fetchFailureMu.Lock()
	priorFailure := s.fetchFailure
	s.fetchFailureMu.Unlock()
	if priorFailure != nil {
		// Note the cause may name a DIFFERENT ref — it is the first failure
		// of this operation, remembered so the outage is paid once.
		return nil, fmt.Errorf("fetch checkpoint ref %s: skipped, an earlier checkpoint-ref fetch already failed in this operation: %w", refName, priorFailure)
	}
	// Both spellings are asked for, canonical first, because the REMOTE can
	// hold either one. batchPushRefs pushes <ref>:<ref>, keeping the local and
	// remote spellings deliberately identical, so a machine whose shard bucket
	// was folded publishes the checkpoint under the folded name — and a clone
	// that has neither spelling locally (a fresh one, or any case-sensitive
	// filesystem) would otherwise ask only for the canonical name, be told
	// "absent", and never see an intact checkpoint. It also strands a
	// remote-list discovery stub, which ParseRef accepts from the folded remote
	// name and which then has nothing to hydrate from.
	//
	// The second ask costs one extra round-trip on a genuine miss, which is why
	// it runs on the absence signal alone: a transport failure still returns
	// immediately, and is still memoized so an outage is paid once.
	candidates := []plumbing.ReferenceName{refName}
	if folded, ok := FoldedRefName(cid); ok {
		candidates = append(candidates, folded)
	}
	for _, candidate := range candidates {
		fetchErr := s.refFetcher(ctx, candidate)
		if fetchErr == nil {
			// Re-resolve after a successful fetch. ErrReferenceNotFound here
			// means the remote genuinely has no such checkpoint; anything else
			// is a real error. The re-resolve is tolerant for the same reason
			// the first one is: the fetch writes through the name it asked for
			// and lands in the folded bucket like any other write.
			return s.resolveLocalRef(cid)
		}
		if errors.Is(fetchErr, plumbing.ErrReferenceNotFound) {
			// The fetcher probed the remote and it genuinely lacks this
			// spelling (remote.FetchCheckpointRef's absence signal) — absence,
			// not a failure, and per-ref, so it is not memoized.
			logging.Debug(ctx, "git-refs: remote has no such checkpoint ref",
				slog.String("ref", candidate.String()))
			continue
		}
		// Memoize only network verdicts: a cancellation originating from the
		// CALLER's context says nothing about the remote and must not poison
		// later fetches on this store.
		if ctx.Err() == nil {
			s.fetchFailureMu.Lock()
			if s.fetchFailure == nil {
				s.fetchFailure = fetchErr
			}
			s.fetchFailureMu.Unlock()
		}
		logging.Debug(ctx, "git-refs: on-demand checkpoint ref fetch failed",
			slog.String("ref", candidate.String()), slog.String("error", fetchErr.Error()))
		return nil, fmt.Errorf("fetch checkpoint ref %s: %w", candidate, fetchErr)
	}
	return nil, plumbing.ErrReferenceNotFound
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
// recent first.
//
// When the context opts in (WithRemoteListDiscovery) and a remote ref lister is
// configured, it additionally discovers checkpoints that exist on the
// checkpoint remote but have no local ref yet — the "second device sees zero
// checkpoints" case. Discovery is names-only (an ls-remote of
// refs/entire/checkpoints/*, no object transfer): each remote-only checkpoint is
// listed from its ref name alone and hydrated lazily on a later read via the
// on-demand ref fetch. Remote enumeration is best-effort and additive — a
// failure logs and leaves the local results intact rather than failing the
// whole listing.
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
	// indexByID's keys are exactly the checkpoint IDs in checkpoints, and its
	// values index them. Both halves are relied on: the keys answer "is this
	// checkpoint already listed" for the folded-spelling collapse below and for
	// remote discovery, and the index lets a canonical ref replace the entry a
	// folded one contributed. Keeping that in one map is deliberate — a
	// separate presence set has to be written on exactly the paths that append,
	// and the two drifting apart lists a checkpoint twice.
	indexByID := make(map[id.CheckpointID]int)
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		cid, ok := ParseRef(ref.Name())
		if !ok {
			return nil
		}
		// One checkpoint can be named by two refs — the canonical spelling and
		// the case-folded one — in a repo a pre-fix CLI forked before
		// resolveLocalRef and writeRefName existed. Both parse to this same ID,
		// so list it once, preferring the canonical spelling because that is
		// the one resolveLocalRef tries first: a listing that disagreed with
		// the read would show one commit and explain another.
		prior, dup := indexByID[cid]
		if dup && !isCanonicalRefName(cid, ref.Name()) {
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
		info := readCommittedInfoFromCheckpointTree(cid, tree)
		if dup {
			checkpoints[prior] = info
			return nil
		}
		indexByID[cid] = len(checkpoints)
		checkpoints = append(checkpoints, info)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate checkpoint refs: %w", err)
	}

	if s.remoteRefLister != nil && remoteListDiscoveryEnabled(ctx) {
		checkpoints = s.appendRemoteDiscovered(ctx, checkpoints, indexByID)
	}

	sortCheckpointInfosByRecency(checkpoints)
	return checkpoints, nil
}

// appendRemoteDiscovered enumerates checkpoint refs on the configured checkpoint
// remote and appends any that are not present locally (tracked in indexByID) as
// not-yet-hydrated CheckpointInfos. A remote listing both spellings of one
// checkpoint's shard is deduplicated by the same map, so the second is skipped
// exactly as a locally-present one is. It never fetches objects: the ref name
// yields the checkpoint ID, and a ULID ID yields its creation time, so a
// discovered checkpoint sorts and displays correctly before its first read
// hydrates the rest. Best-effort: an enumeration failure logs, warns on stderr,
// and returns the unchanged local list.
func (s *gitRefsStore) appendRemoteDiscovered(ctx context.Context, checkpoints []CheckpointInfo, indexByID map[id.CheckpointID]int) []CheckpointInfo {
	remoteRefs, err := s.remoteRefLister(ctx)
	if err != nil {
		logging.Warn(ctx, "git-refs: remote checkpoint enumeration failed; listing local refs only",
			slog.String("error", err.Error()))
		// Match WarnIfMetadataDisconnected: opted-in discovery failing must be
		// visible on stderr — logging.Warn alone lands only in .entire/logs/.
		fmt.Fprintln(os.Stderr, "[entire] Warning: could not reach checkpoint remote; showing local checkpoints only.")
		return checkpoints
	}
	for _, refName := range remoteRefs {
		cid, ok := ParseRef(refName)
		if !ok {
			continue
		}
		if _, dup := indexByID[cid]; dup {
			continue
		}
		indexByID[cid] = len(checkpoints)
		checkpoints = append(checkpoints, remoteDiscoveredInfo(cid))
	}
	return checkpoints
}

// remoteDiscoveredInfo builds the minimal CheckpointInfo for a checkpoint known
// only by its remote ref name. Its contents are not fetched here (that happens
// lazily on read); CreatedAt is recovered from the ULID timestamp so the entry
// sorts by real creation time, and is left zero for a (rare) legacy-hex ref.
// ListedStub marks the entry so hydration can distinguish it from a local ref
// whose root metadata was unreadable (same zero SessionID/SessionCount shape).
func remoteDiscoveredInfo(cid id.CheckpointID) CheckpointInfo {
	info := CheckpointInfo{CheckpointID: cid, ListedStub: true}
	if createdAt, ok := cid.Time(); ok {
		info.CreatedAt = createdAt
	}
	return info
}

// listedCheckpointNeedsHydration reports whether info is a names-only List stub
// that still needs a hydrate attempt. It keys off ListedStub (set by
// remoteDiscoveredInfo), not SessionID/SessionCount zero-ness: a local ref whose
// root metadata.json is missing/unreadable has the same zero fields but must not
// be treated as a stub (hydration can never fix it and would re-fetch forever).
// Callers that need session identity for filtering or display should
// HydrateListedCheckpointInfo first.
func listedCheckpointNeedsHydration(info CheckpointInfo) bool {
	return info.ListedStub && !info.CheckpointID.IsEmpty()
}

// HydrateListedCheckpointInfo fills SessionID/Agent/etc for a List entry that
// was discovered by name only. It reads the checkpoint (triggering an on-demand
// ref fetch when configured) and mirrors the fields List populates for local
// refs via readCommittedInfoFromCheckpointTree, with one deliberate CreatedAt
// divergence: the local List path assigns info.CreatedAt = meta.CreatedAt
// unconditionally, while hydration only overwrites when meta.CreatedAt is
// non-zero (keeping the ULID-derived time from remoteDiscoveredInfo).
//
// Best-effort / fail-once: on Read or last-session metadata failure it logs Warn,
// clears ListedStub so callers do not re-fetch, and returns the original stub
// fields (never a half-hydrated SessionCount-without-SessionID that would poison
// a committedByID cache and still look "done").
func HydrateListedCheckpointInfo(ctx context.Context, store interface {
	Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
	ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, error)
}, info CheckpointInfo) CheckpointInfo {
	if !listedCheckpointNeedsHydration(info) {
		return info
	}
	summary, err := store.Read(ctx, info.CheckpointID)
	if err != nil || summary == nil {
		logging.Warn(ctx, "git-refs: failed to hydrate remote-discovered checkpoint; leaving stub without session metadata",
			slog.String("checkpoint_id", info.CheckpointID.String()),
			slog.String("error", errString(err)))
		info.ListedStub = false // fail-once: do not re-fetch on every list
		return info
	}

	out := info
	out.ListedStub = false
	out.CheckpointsCount = summary.CheckpointsCount
	out.FilesTouched = summary.FilesTouched
	out.SessionCount = len(summary.Sessions)
	out.Imported = summary.Imported
	out.SessionIDs = nil
	lastMetaOK := len(summary.Sessions) == 0
	for i := range summary.Sessions {
		meta, metaErr := store.ReadSessionMetadata(ctx, info.CheckpointID, i)
		if metaErr != nil || meta == nil {
			logging.Warn(ctx, "git-refs: failed to read session metadata while hydrating remote-discovered checkpoint",
				slog.String("checkpoint_id", info.CheckpointID.String()),
				slog.Int("session_index", i),
				slog.String("error", errString(metaErr)))
			continue
		}
		if meta.SessionID != "" {
			out.SessionIDs = append(out.SessionIDs, meta.SessionID)
		}
		if i == len(summary.Sessions)-1 {
			out.Agent = meta.Agent
			out.SessionID = meta.SessionID
			if !meta.CreatedAt.IsZero() {
				out.CreatedAt = meta.CreatedAt
			}
			out.IsTask = meta.IsTask
			out.ToolUseID = meta.ToolUseID
			lastMetaOK = true
		}
	}
	if !lastMetaOK {
		// Avoid caching SessionCount>0 with empty SessionID: that shape no longer
		// needs hydration under the old zero-field heuristic and would poison
		// committedByID / --session filters. Fail-once on the original stub.
		logging.Warn(ctx, "git-refs: remote-discovered checkpoint hydration incomplete; leaving stub without session metadata",
			slog.String("checkpoint_id", info.CheckpointID.String()))
		info.ListedStub = false
		return info
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return "nil result"
	}
	return err.Error()
}

// GetCheckpointAuthor returns the author of the checkpoint ref's tip commit (the
// most recent writer). Returns a zero Author when the ref is absent.
func (s *gitRefsStore) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error) {
	if err := ctx.Err(); err != nil {
		return Author{}, err //nolint:wrapcheck // Propagating context cancellation
	}
	ref, err := s.resolveLocalRef(checkpointID)
	if err != nil {
		return Author{}, nil //nolint:nilerr // invalid ID or no ref → unknown author
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return Author{}, nil //nolint:nilerr // unreadable → unknown author
	}
	return Author{Name: commit.Author.Name, Email: commit.Author.Email}, nil
}
