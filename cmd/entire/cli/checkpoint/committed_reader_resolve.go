package checkpoint

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// CommittedStore provides the production committed checkpoint storage surface.
type CommittedStore interface {
	SessionStore
	MetadataStore
}

// AuthorReader provides optional checkpoint author lookup.
type AuthorReader interface {
	GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error)
}

// ReadCommittedCheckpoint reads a committed checkpoint summary and normalizes
// a nil store response into ErrCheckpointNotFound.
func ReadCommittedCheckpoint(ctx context.Context, reader Reader, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := reader.ReadCheckpoint(ctx, checkpointID)
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
func ReadLatestSessionContent(ctx context.Context, reader SessionReader, checkpointID id.CheckpointID, summary *CheckpointSummary) (*SessionContent, error) {
	if summary == nil || len(summary.Sessions) == 0 {
		return nil, ErrCheckpointNotFound
	}
	latestIndex := len(summary.Sessions) - 1
	content, err := reader.ReadSession(ctx, SessionIndexRef(checkpointID, latestIndex))
	if err != nil {
		return nil, fmt.Errorf("read session %d content: %w", latestIndex, err)
	}
	return content, nil
}

func ReadRawSessionLogForCheckpoint(ctx context.Context, reader interface {
	Reader
	SessionReader
}, checkpointID id.CheckpointID) ([]byte, string, error) {
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
