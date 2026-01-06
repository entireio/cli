package strategy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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

	// Branch is the branch where the session's code commit lives
	Branch string

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
	CheckpointID string

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
	sessionMap := make(map[string]*Session)
	for _, cp := range checkpoints {
		if existing, ok := sessionMap[cp.SessionID]; ok {
			existing.Checkpoints = append(existing.Checkpoints, Checkpoint{
				CheckpointID:     cp.CheckpointID,
				Message:          "Checkpoint: " + cp.CheckpointID,
				Timestamp:        cp.CreatedAt,
				IsTaskCheckpoint: cp.IsTask,
				ToolUseID:        cp.ToolUseID,
			})
		} else {
			// Get description - first try commit message, then fall back to prompt.txt
			description := getDescriptionForCheckpoint(repo, cp.CheckpointID, cp.CreatedAt, cp.Branch)

			sessionMap[cp.SessionID] = &Session{
				ID:          cp.SessionID,
				Description: description,
				Branch:      cp.Branch,
				Strategy:    "", // Will be set from metadata if available
				StartTime:   cp.CreatedAt,
				Checkpoints: []Checkpoint{{
					CheckpointID:     cp.CheckpointID,
					Message:          "Checkpoint: " + cp.CheckpointID,
					Timestamp:        cp.CreatedAt,
					IsTaskCheckpoint: cp.IsTask,
					ToolUseID:        cp.ToolUseID,
				}},
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
					existingCPIDs[cp.CheckpointID] = true
				}
				for _, cp := range addSession.Checkpoints {
					if !existingCPIDs[cp.CheckpointID] {
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

// getDescriptionForCheckpoint gets the description for a checkpoint.
// First tries to find the commit message from a commit with the Entire-Checkpoint trailer,
// then falls back to reading prompt.txt from the entire/sessions branch.
// The metadataCreatedAt is used to optimize the commit search - we only look at commits
// from a few minutes before this timestamp up to now (commits can be rebased to be newer).
// The branchHint is the branch where the commit was originally created, used to prioritize
// searching that branch first (useful for unmerged PRs).
func getDescriptionForCheckpoint(repo *git.Repository, checkpointID string, metadataCreatedAt time.Time, branchHint string) string {
	// First, try to find the commit message
	if commitMsg := findCommitMessageByCheckpointID(repo, checkpointID, metadataCreatedAt, branchHint); commitMsg != "" {
		return commitMsg
	}

	// Fall back to reading from entire/sessions branch (prompt.txt or context.md)
	tree, err := GetMetadataBranchTree(repo)
	if err != nil {
		return NoDescription
	}

	checkpointPath := paths.CheckpointPath(checkpointID)
	return getSessionDescriptionFromTree(tree, checkpointPath)
}

// findCommitMessageByCheckpointID searches for a commit with the given Entire-Checkpoint trailer
// and returns the first line of its commit message.
// The search window is from (metadataCreatedAt - 5 minutes) to now, since:
// - The original commit was created around metadataCreatedAt
// - Rebase/amend can make the commit newer than the metadata timestamp
// - We don't need to look at commits older than a few minutes before the metadata
// If branchHint is provided, searches that branch first before falling back to HEAD.
func findCommitMessageByCheckpointID(repo *git.Repository, checkpointID string, metadataCreatedAt time.Time, branchHint string) string {
	// Search window: commits from 5 minutes before metadata timestamp to now
	// The commit can't be much older than the metadata, but can be arbitrarily newer (after rebase)
	searchLowerBound := metadataCreatedAt.Add(-5 * time.Minute)

	// Try branch hint first if provided
	if branchHint != "" {
		if msg := searchBranchForCheckpoint(repo, branchHint, checkpointID, searchLowerBound); msg != "" {
			return msg
		}
	}

	// Fall back to searching from HEAD
	head, err := repo.Head()
	if err != nil {
		return ""
	}

	return searchCommitsForCheckpoint(repo, head.Hash(), checkpointID, searchLowerBound)
}

// searchBranchForCheckpoint searches a specific branch for a checkpoint.
func searchBranchForCheckpoint(repo *git.Repository, branchName, checkpointID string, searchLowerBound time.Time) string {
	// Try to resolve the branch
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return "" // Branch doesn't exist
	}

	return searchCommitsForCheckpoint(repo, ref.Hash(), checkpointID, searchLowerBound)
}

// searchCommitsForCheckpoint searches commits starting from a given hash.
func searchCommitsForCheckpoint(repo *git.Repository, startHash plumbing.Hash, checkpointID string, searchLowerBound time.Time) string {
	iter, err := repo.Log(&git.LogOptions{
		From:  startHash,
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return ""
	}
	defer iter.Close()

	var foundMsg string
	maxCommitsToScan := 500 // Safety limit

	//nolint:errcheck,gosec // ForEach error handling via sentinel
	iter.ForEach(func(c *object.Commit) error {
		maxCommitsToScan--
		if maxCommitsToScan <= 0 {
			return errors.New("limit reached")
		}

		// Stop if commit is older than our search window
		if c.Committer.When.Before(searchLowerBound) {
			return errors.New("too old")
		}

		// Check if this commit has the matching Entire-Checkpoint trailer
		if cpID, found := paths.ParseCheckpointTrailer(c.Message); found && cpID == checkpointID {
			// Found the commit - extract first line of message
			foundMsg = extractCommitSubject(c.Message)
			return errors.New("found")
		}

		return nil
	})

	return foundMsg
}

// extractCommitSubject returns the first line of a commit message,
// excluding any trailing Entire-Checkpoint trailer if it's on the first line.
func extractCommitSubject(message string) string {
	lines := strings.SplitN(message, "\n", 2)
	if len(lines) == 0 {
		return ""
	}

	subject := strings.TrimSpace(lines[0])

	// If the subject line is just the trailer, return empty (let fallback handle it)
	if strings.HasPrefix(subject, paths.CheckpointTrailerKey+":") {
		return ""
	}

	return subject
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
