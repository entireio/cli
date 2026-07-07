package checkpoint

import (
	"context"
	"errors"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
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
// List unions both backends (disjoint ID spaces). Writes are NOT kind-routed:
// they go to the configured primary (+ mirrors) via writer, since a new
// checkpoint's ID is already minted to match the primary's format
// (see checkpoint.GenerateCheckpointID).
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

// Write is not kind-routed: it targets the configured primary (+ mirrors).
func (s *kindRoutingStore) Write(ctx context.Context, req WriteRequest) error {
	return s.writer.Write(ctx, req) //nolint:wrapcheck // primary error is the operation's error, surfaced verbatim
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
