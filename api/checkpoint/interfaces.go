package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// CheckpointReader provides read access to checkpoint-level persistent data.
//
//nolint:revive // CheckpointReader stutter is accepted — the name marks the checkpoint (vs session) read tier.
type CheckpointReader interface {
	Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
	List(ctx context.Context) ([]CheckpointInfo, error)
}

// SessionReader provides read access to session-level data within a checkpoint.
type SessionReader interface {
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
	ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, error)
	ReadSessionPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error)
	ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, string, error)
}

// PersistentStore provides the production persistent checkpoint storage surface:
// checkpoint-level reads, session-level reads, and the unified Write. Writes go
// through Writer.Write(ctx, WriteRequest); the concrete per-operation methods
// live on the git implementation as the methods Write dispatches to.
type PersistentStore interface {
	CheckpointReader
	SessionReader
	Writer
}

// WriteRequest is a single persistent-store write command. The set is closed to
// other packages: only types in this package can implement it, sealed via the
// unexported isWriteRequest marker. A store dispatches on the concrete type; a
// mirror/fan-out store forwards the same value to each backend's Write.
//
// Four requests are session-level (Session, SessionTranscript, SessionSummary,
// SessionEntityDeltas) and one is checkpoint-level (CheckpointAttribution).
// Adding a write operation is a new request type plus one dispatch case — the
// Store interface stays unchanged and existing backends keep compiling.
type WriteRequest interface {
	isWriteRequest()
}

// Session creates or replaces a session document within a checkpoint,
// materializing the checkpoint on its first session. (session-level)
type Session WriteOptions

// SessionTranscript replaces a session's transcript, prompts, and skill events
// at stop time without clobbering sibling fields. (session-level)
type SessionTranscript UpdateOptions

// SessionSummary rewrites only the summary of the checkpoint's latest session.
// (session-level)
type SessionSummary struct {
	CheckpointID id.CheckpointID
	Summary      *Summary
}

// CheckpointAttribution rewrites the checkpoint root's combined attribution
// across all sessions. (checkpoint-level)
//
//nolint:revive // CheckpointAttribution stutter is accepted — the name makes the checkpoint (vs session) tier explicit.
type CheckpointAttribution struct {
	CheckpointID id.CheckpointID
	Attribution  *Attribution
}

// SessionEntityDeltas records which code entities changed during one session,
// as a document written into that session's directory inside an EXISTING
// checkpoint. (session-level)
//
// Like CheckpointAttribution it is a post-hoc backfill: the document is
// produced out of band, after the checkpoint has been written, so nothing on
// the checkpoint-creation path ever waits on (or can be broken by) it.
type SessionEntityDeltas struct {
	CheckpointID id.CheckpointID
	// SessionID selects which of the checkpoint's session directories receives
	// the document. A checkpoint can hold several sessions and each has its own
	// deltas, so the index is resolved from this ID, never assumed.
	SessionID string
	// Document is the encoded entity_deltas.json payload. Its schema is owned by
	// the producer side (strategy.entityDeltasDocument); the store keeps it
	// opaque and stores it verbatim, subject only to the standard blob
	// redaction every checkpoint file gets.
	Document json.RawMessage
}

func (Session) isWriteRequest()               {}
func (SessionTranscript) isWriteRequest()     {}
func (SessionSummary) isWriteRequest()        {}
func (SessionEntityDeltas) isWriteRequest()   {}
func (CheckpointAttribution) isWriteRequest() {}

// Writer is the persistent-store write surface: a single Write that accepts any
// WriteRequest. It is the natural type for mirror fan-out.
type Writer interface {
	Write(ctx context.Context, req WriteRequest) error
}

// ReadCheckpoint reads a checkpoint summary and normalizes a nil store response
// into ErrCheckpointNotFound.
func ReadCheckpoint(ctx context.Context, reader CheckpointReader, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := reader.Read(ctx, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("read persistent checkpoint: %w", err)
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}
	return summary, nil
}

// ReadLatestSessionContent reads the latest session from an already-resolved
// session reader and summary.
func ReadLatestSessionContent(ctx context.Context, reader SessionReader, checkpointID id.CheckpointID, summary *CheckpointSummary) (*SessionContent, error) {
	if summary == nil || len(summary.Sessions) == 0 {
		return nil, ErrCheckpointNotFound
	}
	latestIndex := len(summary.Sessions) - 1
	content, err := reader.ReadSessionContent(ctx, checkpointID, latestIndex)
	if err != nil {
		return nil, fmt.Errorf("read session %d content: %w", latestIndex, err)
	}
	return content, nil
}

// ReadRawSessionLogForCheckpoint reads a checkpoint's latest-session transcript;
// it needs both reader tiers (resolve the checkpoint, then its latest session).
func ReadRawSessionLogForCheckpoint(ctx context.Context, reader interface {
	CheckpointReader
	SessionReader
}, checkpointID id.CheckpointID) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := ReadCheckpoint(ctx, reader, checkpointID)
	if err != nil {
		return nil, "", err
	}

	content, err := ReadLatestSessionContent(ctx, reader, checkpointID, summary)
	if err != nil {
		return nil, "", err
	}
	return content.Transcript, content.Metadata.SessionID, nil
}
