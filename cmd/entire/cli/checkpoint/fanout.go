package checkpoint

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// fanoutStore serves all reads from the primary and fans writes out to the
// primary plus zero or more mirror backends. The primary is the source of
// truth: a write fails only if the primary write fails. Mirror writes are
// best-effort — a mirror failure is logged and swallowed so it can never break
// a checkpoint operation. Mirrors are therefore write-only and may legitimately
// lag the primary (they never receive ref-level mutations such as cleanup
// deletes or pre-push OPF re-redaction); they must not be promoted to a read or
// sync source without separate reconciliation.
type fanoutStore struct {
	primary PersistentStore
	mirrors []Writer
}

// newFanoutStore wraps a primary with mirror write fan-out. With no mirrors it
// returns the primary unchanged, so the common (no-mirror) path keeps the
// concrete store and all of its optional capabilities. When wrapping is needed,
// it preserves the optional AuthorReader capability iff the primary has it.
func newFanoutStore(primary PersistentStore, mirrors []Writer) PersistentStore {
	if len(mirrors) == 0 {
		return primary
	}
	base := &fanoutStore{primary: primary, mirrors: mirrors}
	if author, ok := primary.(AuthorReader); ok {
		return &fanoutStoreWithAuthor{fanoutStore: base, author: author}
	}
	return base
}

// The read methods are pure delegation to the primary; nolint:wrapcheck because
// re-wrapping the primary's errors here would add noise without context (same
// convention as the contract re-exports in aliases.go).

func (s *fanoutStore) Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	return s.primary.Read(ctx, checkpointID) //nolint:wrapcheck // pure delegation to primary
}

func (s *fanoutStore) List(ctx context.Context) ([]CheckpointInfo, error) {
	return s.primary.List(ctx) //nolint:wrapcheck // pure delegation to primary
}

func (s *fanoutStore) ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	return s.primary.ReadSessionContent(ctx, checkpointID, sessionIndex) //nolint:wrapcheck // pure delegation to primary
}

func (s *fanoutStore) ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, error) {
	return s.primary.ReadSessionMetadata(ctx, checkpointID, sessionIndex) //nolint:wrapcheck // pure delegation to primary
}

func (s *fanoutStore) ReadSessionPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error) {
	return s.primary.ReadSessionPrompts(ctx, checkpointID, sessionIndex) //nolint:wrapcheck // pure delegation to primary
}

func (s *fanoutStore) ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, string, error) {
	return s.primary.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex) //nolint:wrapcheck // pure delegation to primary
}

// Write applies to the primary first; only on primary success does it fan out to
// each mirror best-effort. A mirror error is logged and dropped.
func (s *fanoutStore) Write(ctx context.Context, req WriteRequest) error {
	if err := s.primary.Write(ctx, req); err != nil {
		return err //nolint:wrapcheck // primary error is the operation's error, surfaced verbatim
	}
	for i, mirror := range s.mirrors {
		if err := mirror.Write(ctx, req); err != nil {
			logging.Warn(ctx, "checkpoint mirror write failed; primary write succeeded",
				"mirror_index", i, "error", err.Error())
		}
	}
	return nil
}

// fanoutStoreWithAuthor adds the optional AuthorReader capability when the
// wrapped primary supports it, so callers that type-assert the store to
// AuthorReader (e.g. explain's author fallback) keep working through the wrapper.
type fanoutStoreWithAuthor struct {
	*fanoutStore

	author AuthorReader
}

func (s *fanoutStoreWithAuthor) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error) {
	return s.author.GetCheckpointAuthor(ctx, checkpointID) //nolint:wrapcheck // pure delegation to primary
}

var (
	_ PersistentStore = (*fanoutStore)(nil)
	_ PersistentStore = (*fanoutStoreWithAuthor)(nil)
	_ AuthorReader    = (*fanoutStoreWithAuthor)(nil)
)
