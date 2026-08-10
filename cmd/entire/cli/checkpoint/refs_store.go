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
	return s.refTip(cid, ref)
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
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid update options: checkpoint ID is required")
	}

	parentHash, existing, err := s.refBaseForBackfill(ctx, opts.CheckpointID)
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

	parentHash, existing, err := s.refBaseForBackfill(ctx, checkpointID)
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

	parentHash, existing, err := s.refBaseForBackfill(ctx, checkpointID)
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
	s.fetchFailureMu.Lock()
	priorFailure := s.fetchFailure
	s.fetchFailureMu.Unlock()
	if priorFailure != nil {
		// Note the cause may name a DIFFERENT ref — it is the first failure
		// of this operation, remembered so the outage is paid once.
		return nil, fmt.Errorf("fetch checkpoint ref %s: skipped, an earlier checkpoint-ref fetch already failed in this operation: %w", refName, priorFailure)
	}
	if fetchErr := s.refFetcher(ctx, refName); fetchErr != nil {
		if errors.Is(fetchErr, plumbing.ErrReferenceNotFound) {
			// The fetcher probed the remote and it genuinely lacks this ref
			// (remote.FetchCheckpointRef's absence signal) — absence, not a
			// failure, and per-ref, so it is not memoized.
			logging.Debug(ctx, "git-refs: remote has no such checkpoint ref",
				slog.String("ref", refName.String()))
			return nil, plumbing.ErrReferenceNotFound
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
	seen := make(map[id.CheckpointID]struct{})
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		cid, ok := ParseRef(ref.Name())
		if !ok {
			return nil
		}
		commit, commitErr := s.repo.CommitObject(ref.Hash())
		if commitErr != nil {
			recordListScopeIssue(ctx, ListScopeIssueLocalCheckpointUnreadable)
			return nil //nolint:nilerr // skip unreadable refs, keep listing
		}
		tree, treeErr := commit.Tree()
		if treeErr != nil {
			recordListScopeIssue(ctx, ListScopeIssueLocalCheckpointUnreadable)
			return nil //nolint:nilerr // skip unreadable refs, keep listing
		}
		info, complete := readCommittedInfoFromCheckpointTree(cid, tree)
		if !complete {
			recordListScopeIssue(ctx, ListScopeIssueLocalCheckpointUnreadable)
		}
		checkpoints = append(checkpoints, info)
		seen[cid] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate checkpoint refs: %w", err)
	}

	if s.remoteRefLister != nil && remoteListDiscoveryEnabled(ctx) {
		checkpoints = s.appendRemoteDiscovered(ctx, checkpoints, seen)
	}

	sortCheckpointInfosByRecency(checkpoints)
	return checkpoints, nil
}

// appendRemoteDiscovered enumerates checkpoint refs on the configured checkpoint
// remote and appends any that are not present locally (tracked in seen) as
// not-yet-hydrated CheckpointInfos. It never fetches objects: the ref name
// yields the checkpoint ID, and a ULID ID yields its creation time, so a
// discovered checkpoint sorts and displays correctly before its first read
// hydrates the rest. Best-effort: an enumeration failure logs, warns on stderr,
// and returns the unchanged local list.
func (s *gitRefsStore) appendRemoteDiscovered(ctx context.Context, checkpoints []CheckpointInfo, seen map[id.CheckpointID]struct{}) []CheckpointInfo {
	remoteRefs, err := s.remoteRefLister(ctx)
	if err != nil {
		logging.Warn(ctx, "git-refs: remote checkpoint enumeration failed; listing local refs only",
			slog.String("error", err.Error()))
		// Match WarnIfMetadataDisconnected: opted-in discovery failing must be
		// visible on stderr — logging.Warn alone lands only in .entire/logs/.
		if !recordListScopeIssue(ctx, ListScopeIssueRemoteEnumerationFailed) {
			fmt.Fprintln(os.Stderr, "[entire] Warning: could not reach checkpoint remote; showing local checkpoints only.")
		}
		return checkpoints
	}
	for _, refName := range remoteRefs {
		cid, ok := ParseRef(refName)
		if !ok {
			continue
		}
		if _, dup := seen[cid]; dup {
			continue
		}
		seen[cid] = struct{}{}
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
	// A routing store needs to preserve the source that produced the stub.
	// Delegate before calling Read: normal legacy-ID routing may intentionally
	// exclude refs when git-branch is primary, while a ListedStub is proof that
	// this particular record came from refs.
	if hydrator, ok := store.(interface {
		hydrateListedCheckpointInfo(ctx context.Context, info CheckpointInfo) CheckpointInfo
	}); ok {
		return hydrator.hydrateListedCheckpointInfo(ctx, info)
	}
	summary, err := store.Read(ctx, info.CheckpointID)
	if err != nil || summary == nil {
		logging.Warn(ctx, "git-refs: failed to hydrate remote-discovered checkpoint; leaving stub without session metadata",
			slog.String("checkpoint_id", info.CheckpointID.String()),
			slog.String("error", errString(err)))
		info.ListedStub = false // fail-once: do not re-fetch on every list
		recordListScopeIssue(ctx, ListScopeIssueRemoteHydrationFailed)
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
		recordListScopeIssue(ctx, ListScopeIssueRemoteHydrationFailed)
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
