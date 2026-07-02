package checkpoint

import "errors"

// Errors returned by checkpoint operations.
var (
	// ErrCheckpointNotFound is returned when a checkpoint ID doesn't exist.
	ErrCheckpointNotFound = errors.New("checkpoint not found")

	// ErrNoTranscript is returned when a checkpoint exists but has no transcript.
	ErrNoTranscript = errors.New("no transcript found for checkpoint")
)

// CheckpointVersionBranchV1 identifies the branch-backed checkpoint metadata format.
const CheckpointVersionBranchV1 = "branch-v1"

// CheckpointVersionRefsV1 identifies the per-checkpoint-ref checkpoint metadata
// format (one ref per checkpoint at refs/entire/checkpoints/<shard>/<id>). The
// value follows the <family>-v<major> convention (cf. branch-v1) so
// checkpointpolicy.ParseFormat parses it.
const CheckpointVersionRefsV1 = "refs-v1"
