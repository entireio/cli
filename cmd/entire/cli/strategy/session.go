package strategy

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v5"
)

// NoDescription is the default description for sessions without one.
const NoDescription = "No description"

// Session represents a Claude Code session with its checkpoints.
// A session is created when a user runs `claude` and tracks all changes
// made during that interaction.
type Session struct {
	// ID is the unique session identifier (e.g., "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e")
	ID string

	// Description is a human-readable summary of the session
	// (typically the first prompt or derived from commit messages)
	Description string

	// Strategy is the name of the strategy that created this session
	Strategy string

	// StartTime is when the session was started
	StartTime time.Time

	// Checkpoints is the list of save points within this session
	Checkpoints []Checkpoint
}

// Checkpoint represents a save point within a session.
// Checkpoints can be either session-level (on Stop) or task-level (on subagent completion).
type Checkpoint struct {
	// CheckpointID is the stable 12-hex-char identifier for this checkpoint.
	// Used to look up metadata at <id[:2]>/<id[2:]>/ on entire/sessions branch.
	CheckpointID id.CheckpointID

	// Message is the commit message or checkpoint description
	Message string

	// Timestamp is when this checkpoint was created
	Timestamp time.Time

	// IsTaskCheckpoint indicates if this is a task checkpoint (vs a session checkpoint)
	IsTaskCheckpoint bool

	// ToolUseID is the tool use ID for task checkpoints (empty for session checkpoints)
	ToolUseID string
}

// PromptResponse represents a single user prompt and assistant response pair.
type PromptResponse struct {
	// Prompt is the user's message
	Prompt string

	// Response is the assistant's response
	Response string

	// Files is the list of files modified during this prompt/response
	Files []string
}

// CheckpointDetails contains detailed information extracted from a checkpoint's transcript.
// This is used by the explain command to display checkpoint content.
type CheckpointDetails struct {
	// Interactions contains all prompt/response pairs in this checkpoint.
	// For strategies like auto-commit/commit, this typically has one entry.
	// For strategies like shadow, this may have multiple entries.
	Interactions []PromptResponse

	// Files is the aggregate list of all files modified in this checkpoint.
	// This is a convenience field that combines files from all interactions.
	Files []string
}

// ListSessions returns all sessions from the entire/sessions branch,
// plus any additional sessions from strategies implementing SessionSource.
// It automatically discovers all registered strategies and merges their sessions.
func ListSessions() ([]Session, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get checkpoints from the entire/sessions branch
	checkpoints, err := ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoints: %w", err)
	}

	// Group checkpoints by session ID
	// For multi-session checkpoints, expand SessionIDs array so each session gets the checkpoint
	sessionMap := make(map[string]*Session)
	for _, cp := range checkpoints {
		// Determine which session IDs this checkpoint belongs to
		// Multi-session checkpoints have SessionIDs populated; single-session use SessionID
		sessionIDs := cp.SessionIDs
		if len(sessionIDs) == 0 {
			sessionIDs = []string{cp.SessionID}
		}

		for _, sessionID := range sessionIDs {
			if sessionID == "" {
				continue
			}

			// Convert string to typed CheckpointID
			cpID := id.CheckpointID(cp.CheckpointID)

			if existing, ok := sessionMap[sessionID]; ok {
				existing.Checkpoints = append(existing.Checkpoints, Checkpoint{
					CheckpointID:     cpID,
					Message:          "Checkpoint: " + cp.CheckpointID,
					Timestamp:        cp.CreatedAt,
					IsTaskCheckpoint: cp.IsTask,
					ToolUseID:        cp.ToolUseID,
				})
			} else {
				// Get description from the checkpoint tree
				description := getDescriptionForCheckpoint(repo, cp.CheckpointID)

				sessionMap[sessionID] = &Session{
					ID:          sessionID,
					Description: description,
					Strategy:    "", // Will be set from metadata if available
					StartTime:   cp.CreatedAt,
					Checkpoints: []Checkpoint{{
						CheckpointID:     cpID,
						Message:          "Checkpoint: " + cp.CheckpointID,
						Timestamp:        cp.CreatedAt,
						IsTaskCheckpoint: cp.IsTask,
						ToolUseID:        cp.ToolUseID,
					}},
				}
			}
		}
	}

	// Check all registered strategies for additional sessions
	for _, name := range List() {
		strat, stratErr := Get(name)
		if stratErr != nil {
			continue
		}
		source, ok := strat.(SessionSource)
		if !ok {
			continue
		}
		additionalSessions, addErr := source.GetAdditionalSessions()
		if addErr != nil {
			continue // Skip strategies that fail to provide additional sessions
		}
		for _, addSession := range additionalSessions {
			if addSession == nil {
				continue
			}
			if existing, ok := sessionMap[addSession.ID]; ok {
				// Merge checkpoints - deduplicate by CheckpointID
				existingCPIDs := make(map[string]bool)
				for _, cp := range existing.Checkpoints {
					existingCPIDs[cp.CheckpointID.String()] = true
				}
				for _, cp := range addSession.Checkpoints {
					if !existingCPIDs[cp.CheckpointID.String()] {
						existing.Checkpoints = append(existing.Checkpoints, cp)
					}
				}
				// Update start time if additional session is older
				if addSession.StartTime.Before(existing.StartTime) {
					existing.StartTime = addSession.StartTime
				}
				// Use description from additional source if existing is empty
				if existing.Description == "" || existing.Description == NoDescription {
					existing.Description = addSession.Description
				}
			} else {
				// New session from additional source
				sessionMap[addSession.ID] = addSession
			}
		}
	}

	// Convert map to slice
	sessions := make([]Session, 0, len(sessionMap))
	for _, session := range sessionMap {
		// Sort checkpoints within each session by timestamp (most recent first)
		sort.Slice(session.Checkpoints, func(i, j int) bool {
			return session.Checkpoints[i].Timestamp.After(session.Checkpoints[j].Timestamp)
		})
		sessions = append(sessions, *session)
	}

	// Sort sessions by start time (most recent first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})

	return sessions, nil
}

// GetSession finds a session by ID (supports prefix matching).
// Returns ErrNoSession if no matching session is found.
func GetSession(sessionID string) (*Session, error) {
	sessions, err := ListSessions()
	if err != nil {
		return nil, err
	}
	return findSessionByID(sessions, sessionID)
}

// getDescriptionForCheckpoint reads the description for a checkpoint from the entire/sessions branch.
func getDescriptionForCheckpoint(repo *git.Repository, checkpointID string) string {
	tree, err := GetMetadataBranchTree(repo)
	if err != nil {
		return NoDescription
	}

	checkpointPath := paths.CheckpointPath(checkpointID)
	return getSessionDescriptionFromTree(tree, checkpointPath)
}

// findSessionByID finds a session by exact ID or prefix match.
func findSessionByID(sessions []Session, sessionID string) (*Session, error) {
	for _, session := range sessions {
		if session.ID == sessionID || strings.HasPrefix(session.ID, sessionID) {
			return &session, nil
		}
	}
	return nil, ErrNoSession
}
