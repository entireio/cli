package checkpoint

import "errors"

// Errors returned by checkpoint operations.
var (
	// ErrCheckpointNotFound is returned when a checkpoint ID doesn't exist.
	ErrCheckpointNotFound = errors.New("checkpoint not found")

	// ErrNoTranscript is returned when a checkpoint exists but has no transcript.
	ErrNoTranscript = errors.New("no transcript found for checkpoint")

	// ErrTaskNotFound is returned when a checkpoint exists but has no subagent
	// task record with the requested tool_use_id.
	ErrTaskNotFound = errors.New("task record not found")

	// ErrTaskRecordsUnsupported is returned by a TaskReader whose source does
	// not carry subagent task records (e.g. a checkpoint read over the Entire
	// API). Callers treat it as "unknown", not as a read failure.
	ErrTaskRecordsUnsupported = errors.New("subagent task records are not available from this checkpoint source")
)
