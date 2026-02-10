package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/charmbracelet/huh"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GetRewindPoints returns available rewind points.
// Uses checkpoint.GitStore.ListTemporaryCheckpoints for reading from shadow branches.
func (s *ManualCommitStrategy) GetRewindPoints(limit int) ([]RewindPoint, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Get current HEAD to find matching shadow branch
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Find sessions for current HEAD
	sessions, err := s.findSessionsForCommit(head.Hash().String())
	if err != nil {
		// Log error but continue to check for logs-only points
		sessions = nil
	}

	var allPoints []RewindPoint

	// Collect checkpoint points from active sessions using checkpoint.GitStore
	// Cache session prompts by session ID to avoid re-reading the same prompt file
	sessionPrompts := make(map[string]string)

	for _, state := range sessions {
		checkpoints, err := store.ListTemporaryCheckpoints(context.Background(), state.BaseCommit, state.WorktreeID, state.SessionID, limit)
		if err != nil {
			continue // Error reading checkpoints, skip this session
		}

		for _, cp := range checkpoints {
			// Get session prompt (cached by session ID)
			sessionPrompt, ok := sessionPrompts[cp.SessionID]
			if !ok {
				sessionPrompt = readSessionPrompt(repo, cp.CommitHash, cp.MetadataDir)
				sessionPrompts[cp.SessionID] = sessionPrompt
			}

			allPoints = append(allPoints, RewindPoint{
				ID:               cp.CommitHash.String(),
				Message:          cp.Message,
				MetadataDir:      cp.MetadataDir,
				Date:             cp.Timestamp,
				IsTaskCheckpoint: cp.IsTaskCheckpoint,
				ToolUseID:        cp.ToolUseID,
				SessionID:        cp.SessionID,
				SessionPrompt:    sessionPrompt,
				Agent:            state.AgentType,
			})
		}
	}

	// Sort by date, most recent first
	sort.Slice(allPoints, func(i, j int) bool {
		return allPoints[i].Date.After(allPoints[j].Date)
	})

	if len(allPoints) > limit {
		allPoints = allPoints[:limit]
	}

	// Also include logs-only points from commit history
	logsOnlyPoints, err := s.GetLogsOnlyRewindPoints(limit)
	if err == nil && len(logsOnlyPoints) > 0 {
		// Build set of existing point IDs for deduplication
		existingIDs := make(map[string]bool)
		for _, p := range allPoints {
			existingIDs[p.ID] = true
		}

		// Add logs-only points that aren't already in the list
		for _, p := range logsOnlyPoints {
			if !existingIDs[p.ID] {
				allPoints = append(allPoints, p)
			}
		}

		// Re-sort by date
		sort.Slice(allPoints, func(i, j int) bool {
			return allPoints[i].Date.After(allPoints[j].Date)
		})

		// Re-trim to limit
		if len(allPoints) > limit {
			allPoints = allPoints[:limit]
		}
	}

	return allPoints, nil
}

// GetLogsOnlyRewindPoints finds commits in the current branch's history that have
// condensed session logs on the entire/checkpoints/v1 branch. These are commits that
// were created with session data but the shadow branch has been condensed.
//
// The function works by:
// 1. Getting all checkpoints from the entire/checkpoints/v1 branch
// 2. Building a map of checkpoint ID -> checkpoint info
// 3. Scanning the current branch history for commits with Entire-Checkpoint trailers
// 4. Matching by checkpoint ID (stable across amend/rebase)
func (s *ManualCommitStrategy) GetLogsOnlyRewindPoints(limit int) ([]RewindPoint, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, err
	}

	// Get all checkpoints from entire/checkpoints/v1 branch
	checkpoints, err := s.listCheckpoints()
	if err != nil {
		// No checkpoints yet is fine
		return nil, nil //nolint:nilerr // Expected when no checkpoints exist
	}

	if len(checkpoints) == 0 {
		return nil, nil
	}

	// Build map of checkpoint ID -> checkpoint info
	// Checkpoint ID is the stable link from Entire-Checkpoint trailer
	checkpointInfoMap := make(map[id.CheckpointID]CheckpointInfo)
	for _, cp := range checkpoints {
		if !cp.CheckpointID.IsEmpty() {
			checkpointInfoMap[cp.CheckpointID] = cp
		}
	}

	// Get metadata branch tree for reading session prompts (best-effort, ignore errors)
	metadataTree, _ := GetMetadataBranchTree(repo) //nolint:errcheck // Best-effort for session prompts

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Use LogOptions with Order=LogOrderCommitterTime to traverse all parents of merge commits.
	iter, err := repo.Log(&git.LogOptions{
		From:  head.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get commit log: %w", err)
	}

	var points []RewindPoint
	count := 0

	err = iter.ForEach(func(c *object.Commit) error {
		if count >= logsOnlyScanLimit {
			return errStop
		}
		count++

		// Extract checkpoint ID from Entire-Checkpoint trailer (ParseCheckpoint validates format)
		cpID, found := trailers.ParseCheckpoint(c.Message)
		if !found {
			return nil
		}
		// Check if this checkpoint ID has metadata on entire/checkpoints/v1
		cpInfo, found := checkpointInfoMap[cpID]
		if !found {
			return nil
		}

		// Create logs-only rewind point
		message := strings.Split(c.Message, "\n")[0]

		// Read session prompts from metadata tree
		var sessionPrompt string
		var sessionPrompts []string
		if metadataTree != nil {
			checkpointPath := paths.CheckpointPath(cpInfo.CheckpointID) //nolint:staticcheck // already present in codebase
			// For multi-session checkpoints, read all prompts
			if cpInfo.SessionCount > 1 && len(cpInfo.SessionIDs) > 1 {
				sessionPrompts = ReadAllSessionPromptsFromTree(metadataTree, checkpointPath, cpInfo.SessionCount, cpInfo.SessionIDs)
				// Use the last (most recent) prompt as the main session prompt
				if len(sessionPrompts) > 0 {
					sessionPrompt = sessionPrompts[len(sessionPrompts)-1]
				}
			} else {
				sessionPrompt = ReadSessionPromptFromTree(metadataTree, checkpointPath)
				if sessionPrompt != "" {
					sessionPrompts = []string{sessionPrompt}
				}
			}
		}

		points = append(points, RewindPoint{
			ID:             c.Hash.String(),
			Message:        message,
			Date:           c.Author.When,
			IsLogsOnly:     true,
			CheckpointID:   cpInfo.CheckpointID,
			Agent:          cpInfo.Agent,
			SessionID:      cpInfo.SessionID,
			SessionPrompt:  sessionPrompt,
			SessionCount:   cpInfo.SessionCount,
			SessionIDs:     cpInfo.SessionIDs,
			SessionPrompts: sessionPrompts,
		})

		return nil
	})

	if err != nil && !errors.Is(err, errStop) {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	if len(points) > limit {
		points = points[:limit]
	}

	return points, nil
}

// Rewind restores the working directory to a checkpoint.
//

func (s *ManualCommitStrategy) Rewind(point RewindPoint) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get the checkpoint commit
	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get tree: %w", err)
	}

	// Reset the shadow branch to the rewound checkpoint
	// This ensures the next checkpoint will only include prompts from this point forward
	if err := s.resetShadowBranchToCheckpoint(repo, commit); err != nil {
		// Log warning but don't fail - file restoration is the primary operation
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to reset shadow branch: %v\n", err)
	}

	// Load session state to get untracked files that existed at session start
	sessionID, hasSessionTrailer := trailers.ParseSession(commit.Message)
	var preservedUntrackedFiles map[string]bool
	if hasSessionTrailer {
		state, stateErr := s.loadSessionState(sessionID)
		if stateErr == nil && state != nil && len(state.UntrackedFilesAtStart) > 0 {
			preservedUntrackedFiles = make(map[string]bool)
			for _, f := range state.UntrackedFilesAtStart {
				preservedUntrackedFiles[f] = true
			}
		}
	}

	// Build set of files in the checkpoint tree (excluding metadata)
	checkpointFiles := make(map[string]bool)
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasPrefix(f.Name, entireDir) {
			checkpointFiles[f.Name] = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to list checkpoint files: %w", err)
	}

	// Get HEAD tree to identify tracked files
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return fmt.Errorf("failed to get HEAD commit: %w", err)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get HEAD tree: %w", err)
	}

	// Build set of files tracked in HEAD
	trackedFiles := make(map[string]bool)
	//nolint:errcheck // Error is not critical for rewind
	_ = headTree.Files().ForEach(func(f *object.File) error {
		trackedFiles[f.Name] = true
		return nil
	})

	// Get repository root to walk from there
	repoRoot, err := GetWorktreePath()
	if err != nil {
		repoRoot = "." // Fallback to current directory
	}

	// Find and delete untracked files that aren't in the checkpoint
	// These are likely files created by the agent in later checkpoints
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // Skip filesystem errors during walk
		}

		// Get path relative to repo root
		relPath, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil //nolint:nilerr // Skip paths we can't make relative
		}

		// Skip directories and protected paths
		if info.IsDir() {
			if isProtectedPath(relPath) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip files in protected directories
		if isProtectedPath(relPath) {
			return nil
		}

		// If file is in checkpoint, it will be restored
		if checkpointFiles[relPath] {
			return nil
		}

		// If file is tracked in HEAD, don't delete (user's committed work)
		if trackedFiles[relPath] {
			return nil
		}

		// If file existed at session start, preserve it (untracked user files)
		if preservedUntrackedFiles[relPath] {
			return nil
		}

		// File is untracked and not in checkpoint - delete it (use absolute path)
		if removeErr := os.Remove(path); removeErr == nil {
			fmt.Fprintf(os.Stderr, "  Deleted: %s\n", relPath)
		}

		return nil
	})
	if err != nil {
		// Non-fatal - continue with restoration
		fmt.Fprintf(os.Stderr, "Warning: error walking directory: %v\n", err)
	}

	// Restore files from checkpoint
	err = tree.Files().ForEach(func(f *object.File) error {
		// Skip metadata directories - these are for checkpoint storage, not working dir
		if strings.HasPrefix(f.Name, entireDir) {
			return nil
		}

		contents, err := f.Contents()
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", f.Name, err)
		}

		// Ensure directory exists
		dir := filepath.Dir(f.Name)
		if dir != "." {
			//nolint:gosec // G301: Need 0o755 for user directories during rewind
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}
		}

		// Write file with appropriate permissions
		var perm os.FileMode = 0o644
		if f.Mode == filemode.Executable {
			perm = 0o755
		}
		if err := os.WriteFile(f.Name, []byte(contents), perm); err != nil {
			return fmt.Errorf("failed to write file %s: %w", f.Name, err)
		}

		fmt.Fprintf(os.Stderr, "  Restored: %s\n", f.Name)
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to iterate tree files: %w", err)
	}

	fmt.Println()
	if len(point.ID) >= 7 {
		fmt.Printf("Restored files from shadow commit %s\n", point.ID[:7])
	} else {
		fmt.Printf("Restored files from shadow commit %s\n", point.ID)
	}
	fmt.Println()

	return nil
}

// resetShadowBranchToCheckpoint resets the shadow branch HEAD to the given checkpoint.
// This ensures that when the user commits after rewinding, the next checkpoint will only
// include prompts from the rewound point, not prompts from later checkpoints.
func (s *ManualCommitStrategy) resetShadowBranchToCheckpoint(repo *git.Repository, commit *object.Commit) error {
	// Extract session ID from the checkpoint commit's Entire-Session trailer
	sessionID, found := trailers.ParseSession(commit.Message)
	if !found {
		return errors.New("checkpoint has no Entire-Session trailer")
	}

	// Load session state to get the shadow branch name
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}

	// Reset the shadow branch to the checkpoint commit
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)

	// Update the reference to point to the checkpoint commit
	ref := plumbing.NewHashReference(refName, commit.Hash)
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to update shadow branch: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[entire] Reset shadow branch %s to checkpoint %s\n", shadowBranchName, commit.Hash.String()[:7])
	return nil
}

// CanRewind checks if rewinding is possible.
// For manual-commit strategy, rewind restores files from a checkpoint - uncommitted changes are expected
// and will be replaced by the checkpoint contents. Returns true with a warning message showing
// what changes will be reverted.
func (s *ManualCommitStrategy) CanRewind() (bool, string, error) {
	return checkCanRewindWithWarning()
}

// PreviewRewind returns what will happen if rewinding to the given point.
// This allows showing warnings about untracked files that will be deleted.
func (s *ManualCommitStrategy) PreviewRewind(point RewindPoint) (*RewindPreview, error) {
	// Logs-only points don't modify the working directory
	if point.IsLogsOnly {
		return &RewindPreview{}, nil
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get the checkpoint commit
	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// Load session state to get untracked files that existed at session start
	sessionID, hasSessionTrailer := trailers.ParseSession(commit.Message)
	var preservedUntrackedFiles map[string]bool
	if hasSessionTrailer {
		state, stateErr := s.loadSessionState(sessionID)
		if stateErr == nil && state != nil && len(state.UntrackedFilesAtStart) > 0 {
			preservedUntrackedFiles = make(map[string]bool)
			for _, f := range state.UntrackedFilesAtStart {
				preservedUntrackedFiles[f] = true
			}
		}
	}

	// Build set of files in the checkpoint tree (excluding metadata)
	checkpointFiles := make(map[string]bool)
	var filesToRestore []string
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasPrefix(f.Name, entireDir) {
			checkpointFiles[f.Name] = true
			filesToRestore = append(filesToRestore, f.Name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoint files: %w", err)
	}

	// Get HEAD tree to identify tracked files
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD commit: %w", err)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD tree: %w", err)
	}

	// Build set of files tracked in HEAD
	trackedFiles := make(map[string]bool)
	//nolint:errcheck // Error is not critical for preview
	_ = headTree.Files().ForEach(func(f *object.File) error {
		trackedFiles[f.Name] = true
		return nil
	})

	// Get repository root to walk from there
	repoRoot, err := GetWorktreePath()
	if err != nil {
		repoRoot = "."
	}

	// Find untracked files that would be deleted
	var filesToDelete []string
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // Skip filesystem errors during walk
		}

		relPath, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil //nolint:nilerr // Skip paths we can't make relative
		}

		// Skip directories and protected paths
		if info.IsDir() {
			if isProtectedPath(relPath) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip files in protected directories
		if isProtectedPath(relPath) {
			return nil
		}

		// If file is in checkpoint, it will be restored (not deleted)
		if checkpointFiles[relPath] {
			return nil
		}

		// If file is tracked in HEAD, don't delete (user's committed work)
		if trackedFiles[relPath] {
			return nil
		}

		// If file existed at session start, preserve it (untracked user files)
		if preservedUntrackedFiles[relPath] {
			return nil
		}

		// File is untracked and not in checkpoint - will be deleted
		filesToDelete = append(filesToDelete, relPath)
		return nil
	})
	if err != nil {
		// Non-fatal, return what we have
		return &RewindPreview{ //nolint:nilerr // Partial result is still useful
			FilesToRestore: filesToRestore,
			FilesToDelete:  filesToDelete,
		}, nil
	}

	// Sort for consistent output
	sort.Strings(filesToRestore)
	sort.Strings(filesToDelete)

	return &RewindPreview{
		FilesToRestore: filesToRestore,
		FilesToDelete:  filesToDelete,
	}, nil
}

// RestoreLogsOnly restores session logs from a logs-only rewind point.
// This fetches the transcript from entire/checkpoints/v1 and writes it to the agent's session directory.
// Does not modify the working directory.
// When multiple sessions were condensed to the same checkpoint, ALL sessions are restored.
// If force is false, prompts for confirmation when local logs have newer timestamps.
func (s *ManualCommitStrategy) RestoreLogsOnly(point RewindPoint, force bool) error {
	if !point.IsLogsOnly {
		return errors.New("not a logs-only rewind point")
	}

	if point.CheckpointID.IsEmpty() {
		return errors.New("missing checkpoint ID")
	}

	// Resolve agent from checkpoint metadata (fall back to Claude for old checkpoints)
	ag, err := resolveAgentForRewind(point.Agent)
	if err != nil {
		return fmt.Errorf("failed to resolve agent: %w", err)
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Read checkpoint summary to get session count
	summary, err := store.ReadCommitted(context.Background(), point.CheckpointID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if summary == nil {
		return fmt.Errorf("checkpoint not found: %s", point.CheckpointID)
	}

	// Get repo root for agent session directory lookup
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	sessionDir, err := ag.GetSessionDir(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to get agent session directory: %w", err)
	}

	// Ensure session directory exists
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		return fmt.Errorf("failed to create agent session directory: %w", err)
	}

	// Check for newer local logs if not forcing
	if !force {
		sessions := s.classifySessionsForRestore(context.Background(), sessionDir, ag, store, point.CheckpointID, summary)
		hasConflicts := false
		for _, sess := range sessions {
			if sess.Status == StatusLocalNewer {
				hasConflicts = true
				break
			}
		}
		if hasConflicts {
			shouldOverwrite, promptErr := PromptOverwriteNewerLogs(sessions)
			if promptErr != nil {
				return promptErr
			}
			if !shouldOverwrite {
				fmt.Fprintf(os.Stderr, "Resume cancelled. Local session logs preserved.\n")
				return nil
			}
		}
	}

	// Count sessions to restore
	totalSessions := len(summary.Sessions)
	if totalSessions > 1 {
		fmt.Fprintf(os.Stderr, "Restoring %d sessions from checkpoint:\n", totalSessions)
	}

	// Restore all sessions (oldest to newest, using 0-based indexing)
	for i := range totalSessions {
		content, readErr := store.ReadSessionContent(context.Background(), point.CheckpointID, i)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "  Warning: failed to read session %d: %v\n", i, readErr)
			continue
		}
		if content == nil || len(content.Transcript) == 0 {
			continue
		}

		sessionID := content.Metadata.SessionID
		if sessionID == "" {
			// Fallback: can't identify session without ID
			fmt.Fprintf(os.Stderr, "  Warning: session %d has no session ID, skipping\n", i)
			continue
		}

		sessionFile := resolveSessionFilePath(sessionID, ag, sessionDir)

		// Get first prompt for display
		promptPreview := ExtractFirstPrompt(content.Prompts)

		if totalSessions > 1 {
			isLatest := i == totalSessions-1
			if promptPreview != "" {
				if isLatest {
					fmt.Fprintf(os.Stderr, "  Session %d (latest): %s\n", i+1, promptPreview)
				} else {
					fmt.Fprintf(os.Stderr, "  Session %d: %s\n", i+1, promptPreview)
				}
			}
			fmt.Fprintf(os.Stderr, "    Writing to: %s\n", sessionFile)
		} else {
			fmt.Fprintf(os.Stderr, "Writing transcript to: %s\n", sessionFile)
		}

		// Ensure parent directory exists (session file may be in a different dir than sessionDir)
		if mkdirErr := os.MkdirAll(filepath.Dir(sessionFile), 0o750); mkdirErr != nil {
			fmt.Fprintf(os.Stderr, "    Warning: failed to create directory: %v\n", mkdirErr)
			continue
		}

		if writeErr := os.WriteFile(sessionFile, content.Transcript, 0o600); writeErr != nil {
			if totalSessions > 1 {
				fmt.Fprintf(os.Stderr, "    Warning: failed to write transcript: %v\n", writeErr)
				continue
			}
			return fmt.Errorf("failed to write transcript: %w", writeErr)
		}
	}

	return nil
}

// resolveAgentForRewind resolves the agent from checkpoint metadata.
// Falls back to the default agent (Claude) for old checkpoints that lack agent info.
//

func resolveAgentForRewind(agentType agent.AgentType) (agent.Agent, error) {
	if !isSpecificAgentType(agentType) {
		ag := agent.Default()
		if ag == nil {
			return nil, errors.New("no default agent registered")
		}
		return ag, nil
	}
	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		return nil, fmt.Errorf("resolving agent %q: %w", agentType, err)
	}
	return ag, nil
}

// resolveSessionFilePath determines the correct file path for writing an agent's session transcript.
// Checks session state for transcript_path first (needed for agents like Gemini that store
// transcripts at paths that GetSessionDir can't reconstruct, e.g. SHA-256 hashed directories).
// Falls back to the agent's GetSessionDir + ResolveSessionFile.
func resolveSessionFilePath(sessionID string, ag agent.Agent, fallbackSessionDir string) string {
	state, err := LoadSessionState(sessionID)
	if err == nil && state != nil && state.TranscriptPath != "" {
		return state.TranscriptPath
	}

	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	return ag.ResolveSessionFile(fallbackSessionDir, agentSessionID)
}

// readSessionPrompt reads the first prompt from the session's prompt.txt file stored in git.
// Returns an empty string if the prompt cannot be read.
func readSessionPrompt(repo *git.Repository, commitHash plumbing.Hash, metadataDir string) string {
	// Get the commit and its tree
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return ""
	}

	tree, err := commit.Tree()
	if err != nil {
		return ""
	}

	// Look for prompt.txt in the metadata directory
	promptPath := metadataDir + "/" + paths.PromptFileName
	promptEntry, err := tree.File(promptPath)
	if err != nil {
		return ""
	}

	content, err := promptEntry.Contents()
	if err != nil {
		return ""
	}

	return ExtractFirstPrompt(content)
}

// SessionRestoreStatus represents the status of a session being restored.
type SessionRestoreStatus int

const (
	StatusNew             SessionRestoreStatus = iota // Local file doesn't exist
	StatusUnchanged                                   // Local and checkpoint are the same
	StatusCheckpointNewer                             // Checkpoint has newer entries
	StatusLocalNewer                                  // Local has newer entries (conflict)
)

// SessionRestoreInfo contains information about a session being restored.
type SessionRestoreInfo struct {
	SessionID      string
	Prompt         string               // First prompt preview for display
	Status         SessionRestoreStatus // Status of this session
	LocalTime      time.Time
	CheckpointTime time.Time
}

// classifySessionsForRestore checks all sessions in a checkpoint and returns info
// about each session, including whether local logs have newer timestamps.
func (s *ManualCommitStrategy) classifySessionsForRestore(ctx context.Context, sessionDir string, ag agent.Agent, store cpkg.Store, checkpointID id.CheckpointID, summary *cpkg.CheckpointSummary) []SessionRestoreInfo {
	var sessions []SessionRestoreInfo

	totalSessions := len(summary.Sessions)
	// Check all sessions (0-based indexing)
	for i := range totalSessions {
		content, err := store.ReadSessionContent(ctx, checkpointID, i)
		if err != nil || content == nil || len(content.Transcript) == 0 {
			continue
		}

		sessionID := content.Metadata.SessionID
		if sessionID == "" {
			continue
		}

		localPath := resolveSessionFilePath(sessionID, ag, sessionDir)

		localTime := paths.GetLastTimestampFromFile(localPath)
		checkpointTime := paths.GetLastTimestampFromBytes(content.Transcript)
		status := ClassifyTimestamps(localTime, checkpointTime)

		sessions = append(sessions, SessionRestoreInfo{
			SessionID:      sessionID,
			Prompt:         ExtractFirstPrompt(content.Prompts),
			Status:         status,
			LocalTime:      localTime,
			CheckpointTime: checkpointTime,
		})
	}

	return sessions
}

// ClassifyTimestamps determines the restore status based on local and checkpoint timestamps.
func ClassifyTimestamps(localTime, checkpointTime time.Time) SessionRestoreStatus {
	// Local file doesn't exist (no timestamp found)
	if localTime.IsZero() {
		return StatusNew
	}

	// Can't determine checkpoint time - treat as new/safe
	if checkpointTime.IsZero() {
		return StatusNew
	}

	// Compare timestamps
	if localTime.After(checkpointTime) {
		return StatusLocalNewer
	}
	if checkpointTime.After(localTime) {
		return StatusCheckpointNewer
	}
	return StatusUnchanged
}

// StatusToText returns a human-readable status string.
func StatusToText(status SessionRestoreStatus) string {
	switch status {
	case StatusNew:
		return "(new)"
	case StatusUnchanged:
		return "(unchanged)"
	case StatusCheckpointNewer:
		return "(checkpoint is newer)"
	case StatusLocalNewer:
		return "(local is newer)" // shouldn't appear in non-conflict list
	default:
		return ""
	}
}

// PromptOverwriteNewerLogs asks the user for confirmation to overwrite local
// session logs that have newer timestamps than the checkpoint versions.
func PromptOverwriteNewerLogs(sessions []SessionRestoreInfo) (bool, error) {
	// Separate conflicting and non-conflicting sessions
	var conflicting, nonConflicting []SessionRestoreInfo
	for _, s := range sessions {
		if s.Status == StatusLocalNewer {
			conflicting = append(conflicting, s)
		} else {
			nonConflicting = append(nonConflicting, s)
		}
	}

	fmt.Fprintf(os.Stderr, "\nWarning: Local session log(s) have newer entries than the checkpoint:\n")
	for _, info := range conflicting {
		// Show prompt if available, otherwise fall back to session ID
		if info.Prompt != "" {
			fmt.Fprintf(os.Stderr, "  \"%s\"\n", info.Prompt)
		} else {
			fmt.Fprintf(os.Stderr, "  Session: %s\n", info.SessionID)
		}
		fmt.Fprintf(os.Stderr, "    Local last entry:      %s\n", info.LocalTime.Local().Format("2006-01-02 15:04:05"))
		fmt.Fprintf(os.Stderr, "    Checkpoint last entry: %s\n", info.CheckpointTime.Local().Format("2006-01-02 15:04:05"))
	}

	// Show non-conflicting sessions with their status
	if len(nonConflicting) > 0 {
		fmt.Fprintf(os.Stderr, "\nThese other session(s) will also be restored:\n")
		for _, info := range nonConflicting {
			statusText := StatusToText(info.Status)
			if info.Prompt != "" {
				fmt.Fprintf(os.Stderr, "  \"%s\" %s\n", info.Prompt, statusText)
			} else {
				fmt.Fprintf(os.Stderr, "  Session: %s %s\n", info.SessionID, statusText)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\nOverwriting will lose the newer local entries.\n\n")

	var confirmed bool
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Overwrite local session logs with checkpoint versions?").
				Value(&confirmed),
		),
	)
	if isAccessibleMode() {
		form = form.WithAccessible(true)
	}

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get confirmation: %w", err)
	}

	return confirmed, nil
}
