package strategy

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5/plumbing"
)

// GetSessionLog returns the session transcript and session ID.
func (s *ManualCommitStrategy) GetSessionLog(commitHash string) ([]byte, string, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, "", fmt.Errorf("failed to open git repository: %w", err)
	}

	// Try to look up by checkpoint ID first (12 hex chars)
	if len(commitHash) == 12 {
		log, err := s.getCheckpointLog(commitHash)
		if err == nil {
			// Find session ID from metadata (best-effort lookup)
			checkpoints, _ := s.listCheckpoints() //nolint:errcheck // Best-effort session ID lookup
			for _, cp := range checkpoints {
				if cp.CheckpointID == commitHash {
					return log, cp.SessionID, nil
				}
			}
			return log, "", nil
		}
	}

	// Try to look up by commit
	hash, err := repo.ResolveRevision(plumbing.Revision(commitHash))
	if err == nil {
		commit, err := repo.CommitObject(*hash)
		if err == nil {
			// Check for Entire-Checkpoint trailer
			checkpointID, hasTrailer := paths.ParseCheckpointTrailer(commit.Message)
			if hasTrailer && checkpointID != "" {
				log, logErr := s.getCheckpointLog(checkpointID)
				if logErr == nil {
					// Find session ID
					checkpoints, _ := s.listCheckpoints() //nolint:errcheck // Best-effort session ID lookup
					for _, cp := range checkpoints {
						if cp.CheckpointID == checkpointID {
							return log, cp.SessionID, nil
						}
					}
					return log, "", nil
				}
			}

			// Fall back to Entire-Session trailer
			sessionID, found := paths.ParseSessionTrailer(commit.Message)
			if found {
				log, err := s.getSessionLogBySessionID(sessionID)
				if err == nil {
					return log, sessionID, nil
				}
			}

			// Try metadata trailer (shadow branch commit)
			metadataDir, found := paths.ParseMetadataTrailer(commit.Message)
			if found {
				sessionID := filepath.Base(metadataDir)
				tree, treeErr := commit.Tree()
				if treeErr == nil {
					// Try current format first, then legacy
					logPath := filepath.Join(metadataDir, paths.TranscriptFileName)
					if file, fileErr := tree.File(logPath); fileErr == nil {
						if content, contentErr := file.Contents(); contentErr == nil {
							return []byte(content), sessionID, nil
						}
					}
					logPath = filepath.Join(metadataDir, paths.TranscriptFileNameLegacy)
					if file, fileErr := tree.File(logPath); fileErr == nil {
						if content, contentErr := file.Contents(); contentErr == nil {
							return []byte(content), sessionID, nil
						}
					}
				}
			}
		}
	}

	// Try as session ID (commitHash is actually a sessionID)
	log, err := s.getSessionLogBySessionID(commitHash)
	if err != nil {
		return nil, "", err
	}
	return log, commitHash, nil
}

// getSessionLogBySessionID returns all transcripts for a session.
func (s *ManualCommitStrategy) getSessionLogBySessionID(sessionID string) ([]byte, error) {
	checkpoints, err := s.getCheckpointsForSession(sessionID)
	if err != nil {
		// Fall back to active session shadow branch
		return s.getSessionLogFromShadow(sessionID)
	}

	// Sort by time (oldest first)
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.Before(checkpoints[j].CreatedAt)
	})

	var result []byte
	for i, cp := range checkpoints {
		log, err := s.getCheckpointLog(cp.CheckpointID)
		if err != nil {
			continue
		}
		if i > 0 {
			result = append(result, []byte("\n\n--- Next commit ---\n\n")...)
		}
		result = append(result, log...)
	}

	if len(result) == 0 {
		return s.getSessionLogFromShadow(sessionID)
	}

	return result, nil
}

// getSessionLogFromShadow gets transcript from active shadow branch.
func (s *ManualCommitStrategy) getSessionLogFromShadow(sessionID string) ([]byte, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	state, err := s.loadSessionState(sessionID)
	if err != nil || state == nil {
		return nil, fmt.Errorf("no active session: %s", sessionID)
	}

	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get shadow branch reference: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	metadataDir := paths.SessionMetadataDir(sessionID)
	if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			return []byte(content), nil
		}
	}
	if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileNameLegacy); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			return []byte(content), nil
		}
	}

	return nil, errors.New("no transcript found")
}

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
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit)
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
	if checkpoint.CheckpointID == "" {
		return ""
	}
	return paths.MetadataBranchName + ":" + paths.CheckpointPath(checkpoint.CheckpointID)
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
	return paths.FormatSourceRefTrailer(paths.MetadataBranchName, ref.Hash().String())
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
	if checkpoint.CheckpointID == "" {
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
		if description := s.getDescriptionFromShadowBranch(state.SessionID, state.BaseCommit); description != "" {
			session.Description = description
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// getDescriptionFromShadowBranch reads the session description from the shadow branch.
// sessionID is expected to be an Entire session ID (already date-prefixed like "2026-01-12-abc123").
func (s *ManualCommitStrategy) getDescriptionFromShadowBranch(sessionID, baseCommit string) string {
	repo, err := OpenRepository()
	if err != nil {
		return ""
	}

	shadowBranchName := getShadowBranchNameForCommit(baseCommit)
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

	// Use SessionMetadataDirFromEntireID since sessionID is already an Entire session ID
	// (with date prefix like "2026-01-12-abc123")
	metadataDir := paths.SessionMetadataDirFromEntireID(sessionID)
	return getSessionDescriptionFromTree(tree, metadataDir)
}

// Compile-time check that ManualCommitStrategy implements SessionSource
var _ SessionSource = (*ManualCommitStrategy)(nil)
