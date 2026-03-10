package strategy

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
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
// For manual-commit strategy, returns the sharded path on entire/checkpoints/v1 branch.
func (s *ManualCommitStrategy) GetMetadataRef(_ context.Context, checkpoint Checkpoint) string {
	if checkpoint.CheckpointID.IsEmpty() {
		return ""
	}
	return paths.MetadataBranchName + ":" + checkpoint.CheckpointID.Path()
}

// GetSessionMetadataRef returns a reference to the most recent metadata commit for a session.
// For manual-commit strategy, metadata lives on the entire/checkpoints/v1 branch.
func (s *ManualCommitStrategy) GetSessionMetadataRef(ctx context.Context, _ string) string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return ""
	}

	// Get the sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	// The tip of entire/checkpoints/v1 contains all condensed sessions
	// Return a reference to it (sessionID is not used as all sessions are on the same branch)
	return trailers.FormatSourceRef(paths.MetadataBranchName, ref.Hash().String())
}

// GetCheckpointLog returns the session transcript for a specific checkpoint.
// For manual-commit strategy, metadata is stored at sharded paths on entire/checkpoints/v1 branch.
func (s *ManualCommitStrategy) GetCheckpointLog(ctx context.Context, checkpoint Checkpoint) ([]byte, error) { //nolint:unparam // []byte is used by callers; lint false positive from test-only usage
	if checkpoint.CheckpointID.IsEmpty() {
		return nil, ErrNoMetadata
	}
	return s.getCheckpointLog(ctx, checkpoint.CheckpointID)
}

// GetAdditionalSessions implements SessionSource interface.
// Returns active sessions from .git/entire-sessions/ that haven't yet been condensed.
func (s *ManualCommitStrategy) GetAdditionalSessions(ctx context.Context) ([]*Session, error) {
	activeStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}

	if len(activeStates) == 0 {
		return nil, nil
	}

	var sessions []*Session
	for _, state := range activeStates {
		session := &Session{
			ID:          state.SessionID,
			Description: NoDescription,
			Strategy:    StrategyNameManualCommit,
			StartTime:   state.StartedAt,
		}

		// Try to get description from shadow branch
		if description := s.getDescriptionFromShadowBranch(ctx, state.SessionID, state.BaseCommit, state.WorktreeID); description != "" {
			session.Description = description
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// getDescriptionFromShadowBranch reads the session description from the shadow branch.
// sessionID is expected to be an Entire session ID (already date-prefixed like "2026-01-12-abc123").
func (s *ManualCommitStrategy) getDescriptionFromShadowBranch(ctx context.Context, sessionID, baseCommit, worktreeID string) string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return ""
	}

	shadowBranchName := getShadowBranchNameForCommit(baseCommit, worktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return ""
	}

	tree, err := commit.Tree()
	if err != nil {
		return ""
	}

	// Use the session ID directly as the metadata directory name
	metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
	return getSessionDescriptionFromTree(tree, metadataDir)
}
