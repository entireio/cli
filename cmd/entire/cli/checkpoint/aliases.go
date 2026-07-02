package checkpoint

import (
	"context"

	apicheckpoint "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// The persistent-checkpoint contract (persisted document types, option types,
// reader/writer interfaces, and the Write request union) lives in the
// api/checkpoint package so storage backends can depend on it without the CLI's
// agent/git machinery. These aliases re-export it under this package so existing
// CLI call sites are unaffected; the git implementation (GitStore, Open, the
// facade, and the ephemeral shadow-branch surface) stays here.
type (
	// Persisted document types.
	Metadata = apicheckpoint.Metadata
	//nolint:revive // CheckpointSummary stutter is accepted (named to avoid conflict with Summary).
	CheckpointSummary = apicheckpoint.CheckpointSummary
	//nolint:revive // CheckpointInfo stutter is accepted (Info is taken by the generic checkpoint.Info type).
	CheckpointInfo   = apicheckpoint.CheckpointInfo
	SessionContent   = apicheckpoint.SessionContent
	SessionFilePaths = apicheckpoint.SessionFilePaths
	SessionMetrics   = apicheckpoint.SessionMetrics
	Summary          = apicheckpoint.Summary
	LearningsSummary = apicheckpoint.LearningsSummary
	CodeLearning     = apicheckpoint.CodeLearning
	Attribution      = apicheckpoint.Attribution

	// Operation option types.
	WriteOptions               = apicheckpoint.WriteOptions
	UpdateOptions              = apicheckpoint.UpdateOptions
	PrecomputedTranscriptBlobs = apicheckpoint.PrecomputedTranscriptBlobs

	// Reader/writer interfaces and the Write request union. Reads are tiered by
	// scope: CheckpointReader (checkpoint-level) and SessionReader (session-level),
	// composed with Writer into PersistentStore.
	//nolint:revive // CheckpointReader stutter is accepted — marks the checkpoint (vs session) read tier.
	CheckpointReader = apicheckpoint.CheckpointReader
	SessionReader    = apicheckpoint.SessionReader
	PersistentStore  = apicheckpoint.PersistentStore
	Writer           = apicheckpoint.Writer
	WriteRequest     = apicheckpoint.WriteRequest
	// Write request union: session-level (Session, SessionTranscript,
	// SessionSummary) and checkpoint-level (CheckpointAttribution).
	Session           = apicheckpoint.Session
	SessionTranscript = apicheckpoint.SessionTranscript
	SessionSummary    = apicheckpoint.SessionSummary
	//nolint:revive // CheckpointAttribution stutter is accepted — makes the checkpoint (vs session) tier explicit.
	CheckpointAttribution = apicheckpoint.CheckpointAttribution
)

// CheckpointVersionBranchV1 identifies the branch-backed checkpoint metadata format.
const CheckpointVersionBranchV1 = apicheckpoint.CheckpointVersionBranchV1

// CheckpointVersionRefsV1 identifies the per-checkpoint-ref checkpoint metadata format.
const CheckpointVersionRefsV1 = apicheckpoint.CheckpointVersionRefsV1

// Sentinel errors (re-exported so errors.Is keeps working across packages).
var (
	ErrCheckpointNotFound = apicheckpoint.ErrCheckpointNotFound
	ErrNoTranscript       = apicheckpoint.ErrNoTranscript
)

// Contract helper functions, re-exported as thin wrappers rather than vars so
// the facade symbols can't be reassigned by consumers.

func ReadCheckpoint(ctx context.Context, reader CheckpointReader, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	return apicheckpoint.ReadCheckpoint(ctx, reader, checkpointID) //nolint:wrapcheck // thin re-export of the api/checkpoint helper
}

func ReadLatestSessionContent(ctx context.Context, reader SessionReader, checkpointID id.CheckpointID, summary *CheckpointSummary) (*SessionContent, error) {
	return apicheckpoint.ReadLatestSessionContent(ctx, reader, checkpointID, summary) //nolint:wrapcheck // thin re-export of the api/checkpoint helper
}

func ReadRawSessionLogForCheckpoint(ctx context.Context, reader interface {
	CheckpointReader
	SessionReader
}, checkpointID id.CheckpointID) ([]byte, string, error) {
	return apicheckpoint.ReadRawSessionLogForCheckpoint(ctx, reader, checkpointID) //nolint:wrapcheck // thin re-export of the api/checkpoint helper
}
