package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

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
// List unions both backends (disjoint ID spaces). Creates (Session) are NOT
// kind-routed: they go to the configured primary (+ mirrors) via writer, since
// a new checkpoint's ID is already minted to match the primary's format (see
// checkpoint.GenerateCheckpointID). Backfills update an existing checkpoint,
// so they follow the same store order as reads, though only
// ErrCheckpointNotFound falls through (stricter than reads) — see Write.
type kindRoutingStore struct {
	writer      PersistentStore // configured primary + mirrors (fanout); handles Write
	branch      PersistentStore // git-branch store; serves hex reads
	refs        PersistentStore // git-refs store; serves ULID reads (+ hex under refs primary)
	primaryType string
}

// newKindRoutingStore wraps the write fanout plus the two git read stores. It
// preserves the optional AuthorReader capability (explain relies on it) when both
// read stores provide it — the built-in git backends always do.
func newKindRoutingStore(writer, branch, refs PersistentStore, primaryType string) PersistentStore {
	s := &kindRoutingStore{writer: writer, branch: branch, refs: refs, primaryType: primaryType}
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
	branchList, branchErr := s.branch.List(ctx)
	if branchErr != nil {
		recordListScopeIssue(ctx, ListScopeIssueLocalStoreUnreadable)
		branchList = nil
	}
	refsList, refsErr := s.refs.List(ctx)
	if refsErr != nil {
		recordListScopeIssue(ctx, ListScopeIssueLocalStoreUnreadable)
		refsList = nil
	}
	if branchErr != nil && refsErr != nil {
		return nil, errors.Join(branchErr, refsErr)
	}
	// Dedup by ID before sorting. During coexistence/migration the same
	// checkpoint can appear in both backends (a ULID mirrored to the branch, or
	// a hex still on the branch and also migrated into refs). A names-only
	// remote discovery record is deliberately less authoritative than a local,
	// metadata-backed record for the same ID, even if the ULID-derived timestamp
	// happens to be newer than the local metadata timestamp. Conversely, a stub
	// remains preferable to a local record whose metadata could not be read: the
	// stub retains the ability to hydrate from its known refs source.
	byID := make(map[id.CheckpointID]CheckpointInfo, len(branchList)+len(refsList))
	for _, infos := range [][]CheckpointInfo{branchList, refsList} {
		for _, info := range infos {
			current, exists := byID[info.CheckpointID]
			if !exists || preferCheckpointListInfo(info, current) {
				byID[info.CheckpointID] = info
			}
		}
	}
	merged := make([]CheckpointInfo, 0, len(byID))
	for _, info := range byID {
		merged = append(merged, info)
	}
	sortCheckpointInfosByRecency(merged)
	return merged, nil
}

// preferCheckpointListInfo reports whether candidate should replace current
// while coalescing the same checkpoint ID across stores. Readable metadata
// always beats a names-only remote stub. When neither copy has readable
// metadata, retain the stub because it can still hydrate from its known remote
// source; otherwise retain the most recent copy.
func preferCheckpointListInfo(candidate, current CheckpointInfo) bool {
	candidateHasMetadata := checkpointListInfoHasMetadata(candidate)
	currentHasMetadata := checkpointListInfoHasMetadata(current)
	if candidateHasMetadata != currentHasMetadata {
		return candidateHasMetadata
	}
	if candidate.ListedStub != current.ListedStub {
		return candidate.ListedStub
	}
	return candidate.CreatedAt.After(current.CreatedAt)
}

func checkpointListInfoHasMetadata(info CheckpointInfo) bool {
	return info.SessionCount > 0 || info.SessionID != "" || len(info.SessionIDs) > 0
}

// sortCheckpointInfosByRecency orders checkpoints most-recent-first by CreatedAt.
// Shared by the git-branch, git-refs, and routing List implementations so they
// present a consistent order.
func sortCheckpointInfosByRecency(checkpoints []CheckpointInfo) {
	sort.Slice(checkpoints, func(i, j int) bool {
		if checkpoints[i].CreatedAt.Equal(checkpoints[j].CreatedAt) {
			// List order feeds client-side limits. Give equal timestamps a stable
			// tie-break so repeated calls retain the same boundary record.
			return checkpoints[i].CheckpointID.String() > checkpoints[j].CheckpointID.String()
		}
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})
}

// hydrateListedCheckpointInfo preserves the source of a names-only List
// record. Such stubs can only come from the git-refs store, so hydrating one
// must read refs directly rather than applying normal ID-kind routing. In
// particular, a legacy hex ref discovered while git-branch is primary would
// otherwise be looked up only on the branch and incorrectly treated as absent.
func (s *kindRoutingStore) hydrateListedCheckpointInfo(ctx context.Context, info CheckpointInfo) CheckpointInfo {
	return HydrateListedCheckpointInfo(ctx, s.refs, info)
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

// Write routes a create (Session) to the configured primary (+ mirrors): a new
// checkpoint's ID is already minted to match the primary's format (see
// checkpoint.GenerateCheckpointID). Backfills target an EXISTING checkpoint,
// which — like reads — may live in either git backend (e.g. a pre-migration hex
// checkpoint still on the v1 branch under a git-refs primary), so they follow
// the read order, falling through to the next store on ErrCheckpointNotFound.
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
	checkpointID, isBackfill := backfillTarget(req)
	if !isBackfill {
		return s.writer.Write(ctx, req) //nolint:wrapcheck // primary error is the operation's error, surfaced verbatim
	}
	stores := s.backfillOrder(checkpointID)
	var err error
	for i, st := range stores {
		err = st.Write(ctx, req)
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

// backfillTarget returns the checkpoint ID a backfill request updates.
// ok is false for Session (a create) and unknown request types, which are not
// kind-routed.
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
	case CheckpointAttribution:
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
