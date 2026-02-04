package strategy

import (
	"fmt"
	"sort"
	"strings"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5/plumbing"
)

// GetTaskCheckpoint retrieves a task checkpoint.
func (s *ManualCommitStrategy) GetTaskCheckpoint(point RewindPoint) (*TaskCheckpoint, error) {
	return getTaskCheckpointFromTree(point)
}

// GetTaskCheckpointTranscript retrieves the transcript for a task checkpoint.
func (s *ManualCommitStrategy) GetTaskCheckpointTranscript(point RewindPoint) ([]byte, error) {
	return getTaskTranscriptFromTree(point)
}

// GetSessionInfo returns the current session info.
func (s *ManualCommitStrategy) GetSessionInfo() (*SessionInfo, error) {
	repo, err := OpenRepository()
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
	sessions, err := s.findSessionsForCommit(head.Hash().String())
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
// For manual-commit strategy, returns the sharded path on entire/sessions branch.
func (s *ManualCommitStrategy) GetMetadataRef(checkpoint Checkpoint) string {
	if checkpoint.CheckpointID.IsEmpty() {
		return ""
	}
	return paths.MetadataBranchName + ":" + checkpoint.CheckpointID.Path()
}

// GetSessionMetadataRef returns a reference to the most recent metadata commit for a session.
// For manual-commit strategy, metadata lives on the entire/sessions branch.
func (s *ManualCommitStrategy) GetSessionMetadataRef(_ string) string {
	repo, err := OpenRepository()
	if err != nil {
		return ""
	}

	// Get the sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	// The tip of entire/sessions contains all condensed sessions
	// Return a reference to it (sessionID is not used as all sessions are on the same branch)
	return trailers.FormatSourceRef(paths.MetadataBranchName, ref.Hash().String())
}

// GetSessionContext returns the context.md content for a session.
// For manual-commit strategy, reads from the entire/sessions branch using sharded paths.
func (s *ManualCommitStrategy) GetSessionContext(sessionID string) string {
	// Find a checkpoint for this session
	checkpoints, err := s.getCheckpointsForSession(sessionID)
	if err != nil || len(checkpoints) == 0 {
		return ""
	}

	// Use the most recent checkpoint
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})
	checkpointID := checkpoints[0].CheckpointID

	repo, err := OpenRepository()
	if err != nil {
		return ""
	}

	// Get the sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
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

	// Context.md is at <sharded-path>/context.md
	contextPath := paths.CheckpointPath(checkpointID) + "/" + paths.ContextFileName
	file, err := tree.File(contextPath)
	if err != nil {
		return ""
	}
	content, err := file.Contents()
	if err != nil {
		return ""
	}
	return content
}

// GetCheckpointLog returns the session transcript for a specific checkpoint.
// For manual-commit strategy, metadata is stored at sharded paths on entire/sessions branch.
func (s *ManualCommitStrategy) GetCheckpointLog(checkpoint Checkpoint) ([]byte, error) {
	if checkpoint.CheckpointID.IsEmpty() {
		return nil, ErrNoMetadata
	}
	return s.getCheckpointLog(checkpoint.CheckpointID)
}

// GetAdditionalSessions implements SessionSource interface.
// Returns active sessions from .git/entire-sessions/ that haven't yet been condensed.
func (s *ManualCommitStrategy) GetAdditionalSessions() ([]*Session, error) {
	activeStates, err := s.listAllSessionStates()
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
		if description := s.getDescriptionFromShadowBranch(state.SessionID, state.BaseCommit, state.WorktreeID); description != "" {
			session.Description = description
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// getDescriptionFromShadowBranch reads the session description from the shadow branch.
// sessionID is expected to be an Entire session ID (already date-prefixed like "2026-01-12-abc123").
func (s *ManualCommitStrategy) getDescriptionFromShadowBranch(sessionID, baseCommit, worktreeID string) string {
	repo, err := OpenRepository()
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

// Compile-time check that ManualCommitStrategy implements SessionSource
var _ SessionSource = (*ManualCommitStrategy)(nil)
