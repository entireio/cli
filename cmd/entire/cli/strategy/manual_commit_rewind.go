package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"entire.io/cli/cmd/entire/cli/paths"

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
	for _, state := range sessions {
		checkpoints, err := store.ListTemporaryCheckpoints(context.Background(), state.BaseCommit, state.SessionID, limit)
		if err != nil {
			continue // Error reading checkpoints, skip this session
		}

		for _, cp := range checkpoints {
			allPoints = append(allPoints, RewindPoint{
				ID:               cp.CommitHash.String(),
				Message:          cp.Message,
				MetadataDir:      cp.MetadataDir,
				Date:             cp.Timestamp,
				IsTaskCheckpoint: cp.IsTaskCheckpoint,
				ToolUseID:        cp.ToolUseID,
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
// condensed session logs on the entire/sessions branch. These are commits that
// were created with session data but the shadow branch has been condensed.
//
// The function works by:
// 1. Getting all checkpoints from the entire/sessions branch
// 2. Building a map of checkpoint ID -> checkpoint info
// 3. Scanning the current branch history for commits with Entire-Checkpoint trailers
// 4. Matching by checkpoint ID (stable across amend/rebase)
func (s *ManualCommitStrategy) GetLogsOnlyRewindPoints(limit int) ([]RewindPoint, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, err
	}

	// Get all checkpoints from entire/sessions branch
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
	checkpointInfoMap := make(map[string]CheckpointInfo)
	for _, cp := range checkpoints {
		if cp.CheckpointID != "" {
			checkpointInfoMap[cp.CheckpointID] = cp
		}
	}

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

		// Extract checkpoint ID from Entire-Checkpoint trailer
		checkpointID, hasTrailer := paths.ParseCheckpointTrailer(c.Message)
		if !hasTrailer || checkpointID == "" {
			return nil
		}

		// Check if this checkpoint ID has metadata on entire/sessions
		cpInfo, found := checkpointInfoMap[checkpointID]
		if !found {
			return nil
		}

		// Create logs-only rewind point
		message := strings.Split(c.Message, "\n")[0]
		points = append(points, RewindPoint{
			ID:           c.Hash.String(),
			Message:      message,
			Date:         c.Author.When,
			IsLogsOnly:   true,
			CheckpointID: cpInfo.CheckpointID,
			Agent:        cpInfo.Agent,
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
//nolint:maintidx // Complex rewind flow spans multiple recovery modes.
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
	sessionID, hasSessionTrailer := paths.ParseSessionTrailer(commit.Message)
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
	// These are likely files created by Claude in later checkpoints
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // Skip filesystem errors during walk
		}

		// Get path relative to repo root
		relPath, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil //nolint:nilerr // Skip paths we can't make relative
		}

		// Skip directories and special paths
		if info.IsDir() {
			if relPath == gitDir || relPath == claudeDir || relPath == entireDir || strings.HasPrefix(relPath, gitDir+"/") || strings.HasPrefix(relPath, claudeDir+"/") || strings.HasPrefix(relPath, entireDir+"/") {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip if path is a special directory
		if strings.HasPrefix(relPath, gitDir+"/") || strings.HasPrefix(relPath, claudeDir+"/") || strings.HasPrefix(relPath, entireDir+"/") {
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
	sessionID, found := paths.ParseSessionTrailer(commit.Message)
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
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit)
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
	sessionID, hasSessionTrailer := paths.ParseSessionTrailer(commit.Message)
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

		// Skip directories and special paths
		if info.IsDir() {
			if relPath == gitDir || relPath == claudeDir || relPath == entireDir ||
				strings.HasPrefix(relPath, gitDir+"/") ||
				strings.HasPrefix(relPath, claudeDir+"/") ||
				strings.HasPrefix(relPath, entireDir+"/") {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip special directories
		if strings.HasPrefix(relPath, gitDir+"/") ||
			strings.HasPrefix(relPath, claudeDir+"/") ||
			strings.HasPrefix(relPath, entireDir+"/") {
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
// This fetches the transcript from entire/sessions and writes it to Claude's project directory.
// Does not modify the working directory.
func (s *ManualCommitStrategy) RestoreLogsOnly(point RewindPoint) error {
	if !point.IsLogsOnly {
		return errors.New("not a logs-only rewind point")
	}

	if point.CheckpointID == "" {
		return errors.New("missing checkpoint ID")
	}

	// Get transcript from entire/sessions
	content, err := s.getCheckpointLog(point.CheckpointID)
	if err != nil {
		return fmt.Errorf("failed to get checkpoint log: %w", err)
	}

	// Extract session ID from the checkpoint metadata
	sessionID, err := s.getSessionIDFromCheckpoint(point.CheckpointID)
	if err != nil {
		// Fall back to extracting from commit's Entire-Session trailer
		sessionID = s.extractSessionIDFromCommit(point.ID)
		if sessionID == "" {
			return fmt.Errorf("failed to determine session ID: %w", err)
		}
	}

	// Get repo root for Claude project path lookup
	// Use repo root instead of CWD because Claude stores sessions per-repo,
	// and running from a subdirectory would look up the wrong session directory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	claudeProjectDir, err := paths.GetClaudeProjectDir(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to get Claude project directory: %w", err)
	}

	// Ensure project directory exists
	if err := os.MkdirAll(claudeProjectDir, 0o750); err != nil {
		return fmt.Errorf("failed to create Claude project directory: %w", err)
	}

	// Write transcript to Claude's session storage
	modelSessionID := paths.ModelSessionID(sessionID)
	claudeSessionFile := filepath.Join(claudeProjectDir, modelSessionID+".jsonl")

	fmt.Fprintf(os.Stderr, "Writing transcript to: %s\n", claudeSessionFile)
	if err := os.WriteFile(claudeSessionFile, content, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// getSessionIDFromCheckpoint extracts the session ID from a checkpoint's metadata.
func (s *ManualCommitStrategy) getSessionIDFromCheckpoint(checkpointID string) (string, error) {
	repo, err := OpenRepository()
	if err != nil {
		return "", err
	}

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return "", fmt.Errorf("failed to get sessions branch ref: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return "", fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("failed to get commit tree: %w", err)
	}

	// Read metadata.json from the sharded checkpoint folder
	metadataPath := paths.CheckpointPath(checkpointID) + "/metadata.json"
	file, err := tree.File(metadataPath)
	if err != nil {
		return "", fmt.Errorf("failed to get metadata file: %w", err)
	}

	content, err := file.Contents()
	if err != nil {
		return "", fmt.Errorf("failed to read metadata file: %w", err)
	}

	var metadata struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		return "", fmt.Errorf("failed to parse metadata JSON: %w", err)
	}

	return metadata.SessionID, nil
}

// extractSessionIDFromCommit extracts the session ID from a commit's Entire-Session trailer.
func (s *ManualCommitStrategy) extractSessionIDFromCommit(commitHash string) string {
	repo, err := OpenRepository()
	if err != nil {
		return ""
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(commitHash))
	if err != nil {
		return ""
	}

	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return ""
	}

	// Parse Entire-Session trailer
	sessionID, found := paths.ParseSessionTrailer(commit.Message)
	if found {
		return sessionID
	}

	return ""
}
