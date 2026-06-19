package checkpoint

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// CommittedReader provides read access to committed checkpoint data.
type CommittedReader interface {
	ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
}

// CommittedListReader provides read and list access to committed checkpoint data.
type CommittedListReader interface {
	CommittedReader
	ListCommitted(ctx context.Context) ([]CommittedInfo, error)
	ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*CommittedMetadata, error)
	ReadSessionPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error)
}

// CommittedStore provides the production committed checkpoint storage surface.
// Writes go through the unified Writer.Write(ctx, WriteRequest); the concrete
// per-operation methods (WriteCommitted/UpdateCommitted/...) remain on the
// git implementation as the methods Write dispatches to.
type CommittedStore interface {
	CommittedListReader
	ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
	Writer
}

// WriteRequest is a single committed-store write command. The set is closed:
// the only implementations are the request types below, sealed via the
// unexported isWriteRequest marker. A store dispatches on the concrete type;
// a mirror/fan-out store forwards the same value to each backend's Write.
//
// One Store.Write(ctx, req) entry point replaces the former four writer
// methods (WriteCommitted / UpdateCommitted / UpdateSummary /
// UpdateCheckpointSummary), so adding a write operation is a new request type
// plus one dispatch case — the Store interface stays unchanged and existing
// backends keep compiling.
type WriteRequest interface {
	isWriteRequest()
}

// WriteSession creates or replaces a session document within a checkpoint,
// materializing the checkpoint on its first session. (Former WriteCommitted.)
type WriteSession WriteCommittedOptions

// BackfillTranscript replaces a session's transcript, prompts, and skill
// events at stop time without clobbering sibling fields. (Former UpdateCommitted.)
type BackfillTranscript UpdateCommittedOptions

// BackfillSummary rewrites only the summary of the checkpoint's latest
// session. (Former UpdateSummary.)
type BackfillSummary struct {
	CheckpointID id.CheckpointID
	Summary      *Summary
}

// BackfillAttribution rewrites the checkpoint root's combined attribution.
// (Former UpdateCheckpointSummary.)
type BackfillAttribution struct {
	CheckpointID id.CheckpointID
	Attribution  *InitialAttribution
}

func (WriteSession) isWriteRequest()        {}
func (BackfillTranscript) isWriteRequest()  {}
func (BackfillSummary) isWriteRequest()     {}
func (BackfillAttribution) isWriteRequest() {}

// Writer is the committed-store write surface: a single Write that accepts any
// WriteRequest. It is the natural type for mirror fan-out.
type Writer interface {
	Write(ctx context.Context, req WriteRequest) error
}

// ReadCommittedCheckpoint reads a committed checkpoint summary and normalizes
// a nil store response into ErrCheckpointNotFound.
func ReadCommittedCheckpoint(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := reader.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("read committed checkpoint: %w", err)
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}
	return summary, nil
}

// ReadLatestSessionContent reads the latest session from an already-resolved
// committed reader and summary.
func ReadLatestSessionContent(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID, summary *CheckpointSummary) (*SessionContent, error) {
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

func ReadRawSessionLogForCheckpoint(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := ReadCommittedCheckpoint(ctx, reader, checkpointID)
	if err != nil {
		return nil, "", err
	}

	content, err := ReadLatestSessionContent(ctx, reader, checkpointID, summary)
	if err != nil {
		return nil, "", err
	}
	return content.Transcript, content.Metadata.SessionID, nil
}
