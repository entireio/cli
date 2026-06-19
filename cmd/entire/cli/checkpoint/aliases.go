package checkpoint

import (
	"context"

	apicheckpoint "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// The committed-checkpoint contract (persisted document types, option types,
// reader/writer interfaces, and the Write request union) lives in the
// api/checkpoint package so storage backends can depend on it without the
// CLI's agent/git machinery. These aliases re-export it under this package so
// existing CLI call sites are unaffected; the git implementation (GitStore,
// Open, the temporary/shadow-branch types) stays here.
type (
	// Persisted document types.
	CommittedMetadata = apicheckpoint.CommittedMetadata
	//nolint:revive // Named CheckpointSummary to avoid conflict with the Summary struct (matches the contract definition).
	CheckpointSummary  = apicheckpoint.CheckpointSummary
	CommittedInfo      = apicheckpoint.CommittedInfo
	SessionContent     = apicheckpoint.SessionContent
	SessionFilePaths   = apicheckpoint.SessionFilePaths
	SessionMetrics     = apicheckpoint.SessionMetrics
	Summary            = apicheckpoint.Summary
	LearningsSummary   = apicheckpoint.LearningsSummary
	CodeLearning       = apicheckpoint.CodeLearning
	InitialAttribution = apicheckpoint.InitialAttribution

	// Operation option types.
	WriteCommittedOptions      = apicheckpoint.WriteCommittedOptions
	UpdateCommittedOptions     = apicheckpoint.UpdateCommittedOptions
	PrecomputedTranscriptBlobs = apicheckpoint.PrecomputedTranscriptBlobs

	// Reader/writer interfaces and the Write request union.
	CommittedReader     = apicheckpoint.CommittedReader
	CommittedListReader = apicheckpoint.CommittedListReader
	CommittedStore      = apicheckpoint.CommittedStore
	Writer              = apicheckpoint.Writer
	WriteRequest        = apicheckpoint.WriteRequest
	WriteSession        = apicheckpoint.WriteSession
	BackfillTranscript  = apicheckpoint.BackfillTranscript
	BackfillSummary     = apicheckpoint.BackfillSummary
	BackfillAttribution = apicheckpoint.BackfillAttribution
)

// Sentinel errors (re-exported so errors.Is keeps working across packages).
var (
	ErrCheckpointNotFound = apicheckpoint.ErrCheckpointNotFound
	ErrNoTranscript       = apicheckpoint.ErrNoTranscript
)

// Contract helper functions, re-exported as thin wrappers rather than vars so
// the facade symbols can't be reassigned by consumers.

func ReadCommittedCheckpoint(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	return apicheckpoint.ReadCommittedCheckpoint(ctx, reader, checkpointID) //nolint:wrapcheck // thin re-export of the api/checkpoint helper
}

func ReadLatestSessionContent(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID, summary *CheckpointSummary) (*SessionContent, error) {
	return apicheckpoint.ReadLatestSessionContent(ctx, reader, checkpointID, summary) //nolint:wrapcheck // thin re-export of the api/checkpoint helper
}

func ReadRawSessionLogForCheckpoint(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID) ([]byte, string, error) {
	return apicheckpoint.ReadRawSessionLogForCheckpoint(ctx, reader, checkpointID) //nolint:wrapcheck // thin re-export of the api/checkpoint helper
}
