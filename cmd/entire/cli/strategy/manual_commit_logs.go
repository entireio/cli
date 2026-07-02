package strategy

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6/plumbing"
)

// GetTaskCheckpoint retrieves a task checkpoint.
func (s *ManualCommitStrategy) GetTaskCheckpoint(ctx context.Context, point RewindPoint) (*TaskCheckpoint, error) {
	return getTaskCheckpointFromTree(ctx, point)
}

// GetTaskCheckpointTranscript retrieves the transcript for a task checkpoint.
func (s *ManualCommitStrategy) GetTaskCheckpointTranscript(ctx context.Context, point RewindPoint) ([]byte, error) {
	return getTaskTranscriptFromTree(ctx, point)
}

// GetSessionInfo returns the current session info.
func (s *ManualCommitStrategy) GetSessionInfo(ctx context.Context) (*SessionInfo, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	// Check if we're on a shadow branch
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	if head.Name().IsBranch() {
		branchName := head.Name().Short()
		if strings.HasPrefix(branchName, shadowBranchPrefix) {
			return nil, ErrNoSession
		}
	}

	// Find sessions for current HEAD
	sessions, err := s.findSessionsForCommit(ctx, head.Hash().String())
	if err != nil || len(sessions) == 0 {
		return nil, ErrNoSession
	}

	// Return info for most recent session
	state := sessions[0]
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)

	info := &SessionInfo{
		SessionID: state.SessionID,
		Reference: shadowBranchName,
	}

	if ref, err := repo.Reference(refName, true); err == nil {
		info.CommitHash = ref.Hash().String()
	}

	return info, nil
}

// GetMetadataRef returns a reference to the metadata for the given checkpoint.
// For manual-commit strategy, returns the sharded path on refs.Primary.
func (s *ManualCommitStrategy) GetMetadataRef(ctx context.Context, cp Checkpoint) string {
	if cp.CheckpointID.IsEmpty() {
		return ""
	}
	refs := checkpoint.ResolveRefs(ctx)
	return refs.Primary.Short() + ":" + cp.CheckpointID.Path()
}

// GetSessionMetadataRef returns a reference to the most recent metadata commit for a session.
// For manual-commit strategy, metadata lives on refs.Primary.
func (s *ManualCommitStrategy) GetSessionMetadataRef(ctx context.Context, _ string) string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return ""
	}
	defer repo.Close()

	refs := checkpoint.ResolveRefs(ctx)
	ref, err := repo.Reference(refs.Primary, true)
	if err != nil {
		return ""
	}

	// The tip of Primary contains all condensed sessions; return a reference to
	// it (sessionID is not used because all sessions live on the same ref).
	return trailers.FormatSourceRef(refs.Primary.Short(), ref.Hash().String())
}

// GetCheckpointLog returns the session transcript for a specific checkpoint.
// For manual-commit strategy, metadata is stored at sharded paths on entire/checkpoints/v1 branch.
func (s *ManualCommitStrategy) GetCheckpointLog(ctx context.Context, checkpoint Checkpoint) ([]byte, error) { //nolint:unparam // []byte is used by callers; lint false positive from test-only usage
	if checkpoint.CheckpointID.IsEmpty() {
		return nil, ErrNoMetadata
	}
	return s.getCheckpointLog(ctx, checkpoint.CheckpointID)
}
