package checkpoint

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Errors returned by checkpoint operations.
var (
	// ErrCheckpointNotFound is returned when a checkpoint ID doesn't exist.
	ErrCheckpointNotFound = errors.New("checkpoint not found")

	// ErrNoTranscript is returned when a checkpoint exists but has no transcript.
	ErrNoTranscript = errors.New("no transcript found for checkpoint")

	// ErrSessionNotFound is returned when a checkpoint exists but a requested
	// session ID is absent.
	ErrSessionNotFound = errors.New("session not found")
)

// SessionNotFoundError identifies the checkpoint and session that were absent.
type SessionNotFoundError struct {
	CheckpointID id.CheckpointID
	SessionID    string
}

func (e *SessionNotFoundError) Error() string {
	if e.SessionID == "" {
		return fmt.Sprintf("%v in checkpoint %s", ErrSessionNotFound, e.CheckpointID)
	}
	return fmt.Sprintf("%v %q in checkpoint %s", ErrSessionNotFound, e.SessionID, e.CheckpointID)
}

func (e *SessionNotFoundError) Unwrap() error {
	return ErrSessionNotFound
}
