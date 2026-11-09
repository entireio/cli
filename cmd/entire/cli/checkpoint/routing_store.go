package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// kindRoutingStore resolves id-keyed reads across the two git backends so a repo
// running git-refs and git-branch side by side (or mid-migration between them)
// can read checkpoints of BOTH formats without reconfiguring:
//
//   - A ULID checkpoint only ever lives in the git-refs store, so a ULID ID is
//     read from refs and NEVER from the branch (regardless of the active backend).
//   - A legacy-hex ID is read from the active (configured) primary first. When the
//     active primary is git-refs, it also falls back to the git-branch store,
//     because a hex checkpoint may still sit on the pre-migration v1 branch. Under
//     a git-branch primary the branch is authoritative for hex, so refs is not
//     consulted.
//
// List unions both backends (disjoint ID spaces). Fresh creates (Session) are NOT
// kind-routed: they go to the configured primary (+ mirrors) via writer, since
// a new checkpoint's ID is already minted to match the primary's format (see
// checkpoint.GenerateCheckpointID). ReservedSession writes keep a ULID in refs
// under a git-branch primary. Under a git-refs primary, writes to a hex
// checkpoint that is still only on the v1 branch first copy it into its own ref
// (see legacyCopier.copyToRef), because git-refs pre-push never sends v1. Backfills
// update an existing checkpoint, so they follow the same store order as reads,
// though only ErrCheckpointNotFound falls through (stricter than reads) — see
// Write.
type kindRoutingStore struct {
	writer      PersistentStore // configured primary + mirrors (fanout); handles Write
	branch      PersistentStore // git-branch store; serves hex reads
	refs        PersistentStore // git-refs store; serves ULID reads (+ hex under refs primary)
	primaryType string
	// legacy copies a hex checkpoint from the v1 branch into its own ref. It is
	// nil unless both read stores are the built-in git stores, which is always
	// the case outside tests.
	legacy *legacyCopier
}

type legacyCopier struct {
	branch *GitStore
	refs   *gitRefsStore
	// refFetcher asks the remote for a checkpoint's ref before a copy. nil
	// skips the question, so a copy only checks for a local ref.
	refFetcher RefFetchFunc
}

// newKindRoutingStore wraps the write fanout plus the two git read stores. It
// preserves the optional AuthorReader capability (explain relies on it) when both
// read stores provide it — the built-in git backends always do. legacyRefFetcher
// is the remote probe a copy from v1 runs first (see legacyCopier.copyToRef).
func newKindRoutingStore(writer, branch, refs PersistentStore, primaryType string, legacyRefFetcher RefFetchFunc) PersistentStore {
	s := &kindRoutingStore{writer: writer, branch: branch, refs: refs, primaryType: primaryType}
	if b, ok := branch.(*GitStore); ok {
		if r, ok := refs.(*gitRefsStore); ok {
			s.legacy = &legacyCopier{branch: b, refs: r, refFetcher: legacyRefFetcher}
		}
	}
	if _, ok := branch.(AuthorReader); ok {
		if _, ok := refs.(AuthorReader); ok {
			return &kindRoutingStoreWithAuthor{kindRoutingStore: s}
		}
	}
	return s
}

// readOrder returns the stores to consult for checkpointID, in priority order,
// per the routing rules above.
func (s *kindRoutingStore) readOrder(checkpointID id.CheckpointID) []PersistentStore {
	if checkpointID.Kind() == id.KindULID {
		return []PersistentStore{s.refs} // ULIDs only ever live in refs
	}
	switch s.primaryType {
	case BackendTypeGitBranch:
		return []PersistentStore{s.branch} // branch is authoritative for hex
	case BackendTypeGitRefs:
		return []PersistentStore{s.refs, s.branch} // active refs, then pre-migration branch
	default:
		// A non-branch/refs git-backed primary is not a real configuration today;
		// try both git stores so a hex ID still resolves wherever it landed.
		return []PersistentStore{s.branch, s.refs}
	}
}

// firstResolved calls read on each store in order and returns the first genuine
// hit (a non-absent result with no error). A non-final store that reports absent
// OR errors falls through to the next store, so a transient failure in one
// backend (e.g. a git-refs on-demand fetch error) does not hide a checkpoint that
// resolves in the fallback backend. The final store's result is returned as-is
// (hit, absent, or error), so callers still see the backend's own not-found /
// error signal when nothing resolved.
func firstResolved[T any](stores []PersistentStore, read func(PersistentStore) (T, error), absent func(T, error) bool) (T, error) {
	var v T
	var err error
	for i, st := range stores {
		v, err = read(st)
		if i == len(stores)-1 || (err == nil && !absent(v, err)) {
			return v, err
		}
	}
	return v, err
}

// checkpointNotFound reports the checkpoint-level "absent" signal: Read returns
// (nil, nil) — not an error — when a checkpoint does not exist.
func checkpointNotFound(v *CheckpointSummary, err error) bool {
	return err == nil && v == nil
}

// sessionNotFound reports the session-level "absent" signal: the session readers
// return ErrCheckpointNotFound when the checkpoint (or session) is missing.
func sessionNotFound[T any](_ T, err error) bool {
	return errors.Is(err, ErrCheckpointNotFound)
}

func (s *kindRoutingStore) Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	return firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) (*CheckpointSummary, error) { return st.Read(ctx, checkpointID) },
		checkpointNotFound,
	)
}

func (s *kindRoutingStore) List(ctx context.Context) ([]CheckpointInfo, error) {
	branchList, err := s.branch.List(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // in-package store error surfaced verbatim
	}
	refsList, err := s.refs.List(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // in-package store error surfaced verbatim
	}
	merged := make([]CheckpointInfo, 0, len(branchList)+len(refsList))
	merged = append(merged, branchList...)
	merged = append(merged, refsList...)
	sortCheckpointInfosByRecency(merged)
	// Dedup by ID: during coexistence/migration the same checkpoint can appear in
	// both backends (a ULID mirrored to the branch, or a hex still on the branch
	// and also migrated into refs). Keep the first occurrence — i.e. the most
	// recent after the sort.
	deduped := merged[:0]
	seen := make(map[id.CheckpointID]struct{}, len(merged))
	for _, info := range merged {
		if _, dup := seen[info.CheckpointID]; dup {
			continue
		}
		seen[info.CheckpointID] = struct{}{}
		deduped = append(deduped, info)
	}
	return deduped, nil
}

// sortCheckpointInfosByRecency orders checkpoints most-recent-first by CreatedAt.
// Shared by the git-branch, git-refs, and routing List implementations so they
// present a consistent order.
func sortCheckpointInfosByRecency(checkpoints []CheckpointInfo) {
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})
}

func (s *kindRoutingStore) ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	return firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) (*SessionContent, error) {
			return st.ReadSessionContent(ctx, checkpointID, sessionIndex)
		},
		sessionNotFound[*SessionContent],
	)
}

func (s *kindRoutingStore) ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, error) {
	return firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) (*Metadata, error) {
			return st.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
		},
		sessionNotFound[*Metadata],
	)
}

func (s *kindRoutingStore) ReadSessionPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error) {
	return firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) (string, error) {
			return st.ReadSessionPrompts(ctx, checkpointID, sessionIndex)
		},
		sessionNotFound[string],
	)
}

// metaAndPrompts bundles the two non-error returns of ReadSessionMetadataAndPrompts
// so it can flow through the single-value firstResolved helper.
type metaAndPrompts struct {
	meta    *Metadata
	prompts string
}

func (s *kindRoutingStore) ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, string, error) {
	mp, err := firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) (metaAndPrompts, error) {
			m, p, e := st.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
			return metaAndPrompts{meta: m, prompts: p}, e //nolint:wrapcheck // in-package store error surfaced verbatim
		},
		sessionNotFound[metaAndPrompts],
	)
	return mp.meta, mp.prompts, err
}

// ListTasks routes like the session reads: a checkpoint that is absent from
// (or errors in) a non-final store falls through to the next.
func (s *kindRoutingStore) ListTasks(ctx context.Context, checkpointID id.CheckpointID) ([]TaskEntry, error) {
	return firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) ([]TaskEntry, error) {
			return st.ListTasks(ctx, checkpointID)
		},
		sessionNotFound[[]TaskEntry],
	)
}

// taskTranscriptResult carries a definitive per-store answer through
// firstResolved, which otherwise falls through on any error.
type taskTranscriptResult struct {
	transcript []byte
	err        error
}

// ReadTaskTranscript routes like ListTasks, except that a store which holds the
// checkpoint answers definitively: "no such task" or "no transcript stored"
// from the store that has the checkpoint must not fall through and be
// reported by the next store as "checkpoint not found".
func (s *kindRoutingStore) ReadTaskTranscript(ctx context.Context, checkpointID id.CheckpointID, toolUseID string) ([]byte, error) {
	res, err := firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) (taskTranscriptResult, error) {
			transcript, readErr := st.ReadTaskTranscript(ctx, checkpointID, toolUseID)
			if errors.Is(readErr, ErrTaskNotFound) || errors.Is(readErr, ErrNoTranscript) {
				return taskTranscriptResult{err: readErr}, nil
			}
			return taskTranscriptResult{transcript: transcript}, readErr //nolint:wrapcheck // in-package store error surfaced verbatim
		},
		func(taskTranscriptResult, error) bool { return false },
	)
	if err != nil {
		return nil, err
	}
	return res.transcript, res.err
}

// Write routes a create (Session) to the configured primary (+ mirrors): a new
// checkpoint's ID is already minted to match the primary's format (see
// checkpoint.GenerateCheckpointID). Backfills target an EXISTING checkpoint,
// which — like reads — may live in either git backend (e.g. a pre-migration hex
// checkpoint still on the v1 branch under a git-refs primary), so they follow
// the read order, falling through to the next store on ErrCheckpointNotFound.
// Under a git-refs primary, a hex checkpoint the refs store does not have is
// first copied from v1 into its own ref, or its ref fetched from the remote,
// and the backfill retried there, so it is pushed; only a checkpoint that
// cannot be copied (absent from v1, or deleted from this clone) falls through
// to the branch.
//
// The fallthrough is deliberately stricter than read routing's firstResolved
// (which falls through on absent OR any error): only the not-found sentinel
// falls through here. Redirecting a write to another backend after a transient
// primary failure could fork the data, so a hard error aborts and surfaces.
// Note the refs store's backfill absence probe fetches a locally-missing ref
// on demand when a fetcher is wired (refBaseForBackfill), so a checkpoint
// whose ref exists only remotely is fetched and backfilled in place rather
// than falling through to the fallback store.
func (s *kindRoutingStore) Write(ctx context.Context, req WriteRequest) error {
	if reserved, ok := req.(ReservedSession); ok {
		return s.writeReservedSession(ctx, reserved)
	}
	checkpointID, isBackfill := backfillTarget(req)
	if !isBackfill {
		return s.writer.Write(ctx, req) //nolint:wrapcheck // primary error is the operation's error, surfaced verbatim
	}
	stores := s.backfillOrder(checkpointID)
	var err error
	for i, st := range stores {
		err = st.Write(ctx, req)
		if i == 0 && errors.Is(err, ErrCheckpointNotFound) && s.isLegacyUnderRefs(checkpointID) {
			onRef, copyErr := s.copyLegacyUnlessDeleted(ctx, checkpointID)
			if copyErr != nil {
				return copyErr
			}
			if onRef {
				// A retry that still finds nothing (a ref without the
				// checkpoint's metadata) falls through like any other miss.
				err = st.Write(ctx, req)
			}
		}
		if !errors.Is(err, ErrCheckpointNotFound) {
			if err == nil && i > 0 {
				// The most consequential routing decision here: the data landed
				// somewhere other than the configured primary, and mirrors
				// (which follow the primary) were skipped. Record it so "why is
				// this backfill on the v1 branch and not in refs / the mirror"
				// stays diagnosable.
				logging.Info(ctx, "checkpoint: backfill served by fallback store; absent from primary, mirrors skipped",
					slog.String("checkpoint_id", checkpointID.String()),
					slog.String("request_type", fmt.Sprintf("%T", req)))
			}
			return err //nolint:wrapcheck // in-package store error surfaced verbatim
		}
		if i < len(stores)-1 {
			logging.Debug(ctx, "checkpoint: backfill target absent in store, trying next",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("request_type", fmt.Sprintf("%T", req)),
				slog.Int("store_index", i))
		}
	}
	return err //nolint:wrapcheck // ErrCheckpointNotFound from the final store, surfaced verbatim
}

func (s *kindRoutingStore) writeReservedSession(ctx context.Context, req ReservedSession) error {
	checkpointID := WriteOptions(req).CheckpointID
	switch {
	case s.primaryType == BackendTypeGitBranch && checkpointID.Kind() == id.KindULID:
		// Reads never look for a ULID on the branch.
		return s.writeReservedToOriginalBackend(ctx, req, s.refs, BackendTypeGitRefs)
	case s.isLegacyUnderRefs(checkpointID):
		return s.writeReservedLegacyUnderRefs(ctx, req)
	default:
		// The ID matches the primary, or the primary is unrecognised: an unknown
		// primary tells us nothing about which backend minted the ID, and picking
		// one anyway would bypass the configured primary and all of its mirrors.
		return s.writer.Write(ctx, req) //nolint:wrapcheck // primary error is the operation's error, surfaced verbatim
	}
}

// writeReservedLegacyUnderRefs writes a hex checkpoint under a git-refs primary
// through the primary, after copying it from v1 if only v1 has it. This covers
// a session that reserved its ID before the switch, whether or not anything was
// written yet, and attach adding a session to a pre-switch checkpoint.
func (s *kindRoutingStore) writeReservedLegacyUnderRefs(ctx context.Context, req ReservedSession) error {
	checkpointID := WriteOptions(req).CheckpointID
	if s.legacy == nil {
		return s.writeReservedToOriginalBackend(ctx, req, s.branch, BackendTypeGitBranch)
	}
	deleted, err := s.legacy.deleted(checkpointID)
	if err != nil {
		return err
	}
	if deleted {
		// A checkpoint deleted from this clone must not come back as a ref.
		return s.writeReservedToOriginalBackend(ctx, req, s.branch, BackendTypeGitBranch)
	}
	if _, err := s.legacy.copyToRef(ctx, checkpointID); err != nil {
		return err
	}
	return s.writer.Write(ctx, req) //nolint:wrapcheck // primary error is the operation's error, surfaced verbatim
}

// writeReservedToOriginalBackend writes req to target, bypassing the primary:
// a ULID reserved under a git-branch primary goes to refs, and a hex
// checkpoint deleted from this clone goes to the branch so it does not come
// back as a ref.
func (s *kindRoutingStore) writeReservedToOriginalBackend(ctx context.Context, req ReservedSession, target PersistentStore, targetType string) error {
	if err := target.Write(ctx, req); err != nil {
		return err //nolint:wrapcheck // target error is the operation's error, surfaced verbatim
	}
	logging.Info(ctx, "checkpoint: reserved session written to original backend after primary changed",
		slog.String("checkpoint_id", WriteOptions(req).CheckpointID.String()),
		slog.String("target_backend", targetType),
		slog.String("primary_backend", s.primaryType))
	return nil
}

// isLegacyUnderRefs reports whether checkpointID is a hex checkpoint in a repo
// whose primary is git-refs, the case where it may still be only on v1.
func (s *kindRoutingStore) isLegacyUnderRefs(checkpointID id.CheckpointID) bool {
	return s.primaryType == BackendTypeGitRefs && checkpointID.Kind() != id.KindULID
}

// copyLegacyUnlessDeleted runs legacyCopier.copyToRef for a checkpoint this clone
// has not deleted. It reports false when the stores cannot copy.
func (s *kindRoutingStore) copyLegacyUnlessDeleted(ctx context.Context, checkpointID id.CheckpointID) (bool, error) {
	if s.legacy == nil {
		return false, nil
	}
	deleted, err := s.legacy.deleted(checkpointID)
	if err != nil {
		return false, err
	}
	if deleted {
		return false, nil
	}
	return s.legacy.copyToRef(ctx, checkpointID)
}

// copyToRef copies checkpointID from the v1 branch into its own ref, as migration
// would, so later writes advance that ref and pre-push sends it. The write that
// follows queues the ref. Callers check the deleted list first.
//
// It copies only when v1 holds the checkpoint and no ref for it exists, locally
// or, when refFetcher is set, on the remote. An existing local ref may already
// be ahead of v1, and a ref another clone pushed holds that clone's sessions:
// copying anyway would start an unrelated history for it, and pre-push's
// recovery would replay this clone's copy over theirs, last writer wins per
// file. A ref that appears locally while the remote is asked is left alone for
// the same reason (see importOnlyIntoAbsentRef). When the remote cannot be
// asked, copyToRef returns that error and writes nothing, so the write fails
// and is retried later.
//
// onRef reports whether the checkpoint now has its own local ref, so the caller
// should retry its write there: this call copied or fetched it, or it appeared
// after the caller's write missed it (a plain git fetch takes no ref lock). It
// is false only when v1 lacks the checkpoint.
func (c *legacyCopier) copyToRef(ctx context.Context, checkpointID id.CheckpointID) (onRef bool, err error) {
	refName, err := RefName(checkpointID)
	if err != nil {
		return false, err
	}
	exists, err := c.localRefExists(refName)
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}
	subtree, err := c.branch.checkpointSubtreeHash(ctx, checkpointID)
	if errors.Is(err, ErrCheckpointNotFound) || errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	// Diverged v1 copies block only a copy from v1: a ref the remote already
	// holds supersedes both.
	diverged := errors.Is(err, ErrLegacyCheckpointDiverged)
	if err != nil && !diverged {
		return false, fmt.Errorf("read checkpoint %s from %s: %w", checkpointID, c.branch.refs.Primary, err)
	}
	fetched, fetchErr := c.fetchRemoteRef(ctx, refName)
	if fetchErr != nil {
		return false, fetchErr
	}
	if fetched {
		return true, nil
	}
	if diverged {
		return false, fmt.Errorf("read checkpoint %s from %s: %w", checkpointID, c.branch.refs.Primary, err)
	}
	if _, err := importBranchCheckpoint(ctx, c.refs.repo, c.refs, checkpointID, subtree, updatePersistentRef, importOnlyIntoAbsentRef); err != nil {
		return false, err
	}
	logging.Info(ctx, "checkpoint: copied legacy checkpoint from the v1 branch into its own ref",
		slog.String("checkpoint_id", checkpointID.String()))
	return true, nil
}

func (c *legacyCopier) localRefExists(refName plumbing.ReferenceName) (bool, error) {
	return refExists(c.refs.repo, refName)
}

// fetchRemoteRef asks the remote for refName and reports whether the ref now
// exists locally. Without a refFetcher it reports false.
func (c *legacyCopier) fetchRemoteRef(ctx context.Context, refName plumbing.ReferenceName) (bool, error) {
	if c.refFetcher == nil {
		return false, nil
	}
	err := c.refFetcher(ctx, refName)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ask the remote for checkpoint ref %s before copying it from %s: %w", refName, c.branch.refs.Primary, err)
	}
	return c.localRefExists(refName)
}

func (c *legacyCopier) deleted(checkpointID id.CheckpointID) (bool, error) {
	deleted, err := LoadDeletedForRepo(c.refs.repo)
	if err != nil {
		return false, fmt.Errorf("read deleted checkpoints: %w", err)
	}
	return deleted.Contains(checkpointID), nil
}

// backfillTarget returns the checkpoint ID a backfill request updates.
// ok is false for Session and ReservedSession (creates) and unknown request
// types, which are not routed by backfillOrder.
//
// WriteRequest is a closed union: any new backfill-shaped request type MUST be
// added to this switch, or it silently gets create routing — primary-only, no
// fallback — which for a pre-migration checkpoint reintroduces the discarded-
// write bug this routing exists to prevent.
func backfillTarget(req WriteRequest) (id.CheckpointID, bool) {
	switch r := req.(type) {
	case SessionTranscript:
		return r.CheckpointID, true
	case SessionSummary:
		return r.CheckpointID, true
	case Tasks:
		return r.CheckpointID, true
	default:
		return id.EmptyCheckpointID, false
	}
}

// backfillOrder returns the write targets for a backfill of checkpointID, in
// the same priority order reads use. The store that is the configured primary
// is replaced by writer, so a backfill landing on the primary still fans out to
// mirrors; a backfill landing on a fallback store deliberately skips mirrors
// (mirrors follow the primary).
func (s *kindRoutingStore) backfillOrder(checkpointID id.CheckpointID) []PersistentStore {
	order := s.readOrder(checkpointID)
	targets := make([]PersistentStore, len(order))
	for i, st := range order {
		if s.isPrimary(st) {
			targets[i] = s.writer
		} else {
			targets[i] = st
		}
	}
	return targets
}

// isPrimary reports whether st is the configured primary's read store.
func (s *kindRoutingStore) isPrimary(st PersistentStore) bool {
	switch s.primaryType {
	case BackendTypeGitBranch:
		return st == s.branch
	case BackendTypeGitRefs:
		return st == s.refs
	default:
		// Not a real configuration today (buildPrimary only accepts the git
		// backends); backfills would bypass writer and therefore mirrors.
		return false
	}
}

// kindRoutingStoreWithAuthor adds the optional AuthorReader capability, routing
// GetCheckpointAuthor by the same rules as the reads.
type kindRoutingStoreWithAuthor struct {
	*kindRoutingStore
}

func (s *kindRoutingStoreWithAuthor) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error) {
	return firstResolved(s.readOrder(checkpointID),
		func(st PersistentStore) (Author, error) {
			ar, ok := st.(AuthorReader)
			if !ok {
				return Author{}, nil
			}
			return ar.GetCheckpointAuthor(ctx, checkpointID)
		},
		func(a Author, err error) bool { return err == nil && a == Author{} },
	)
}
