package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/trailers"
	"entire.io/cli/cmd/entire/cli/validation"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	// ShadowBranchPrefix is the prefix for shadow branches.
	ShadowBranchPrefix = "entire/"

	// ShadowBranchHashLength is the number of hex characters used in shadow branch names.
	// Shadow branches are named "entire/<hash>" using the first 7 characters of the commit hash.
	ShadowBranchHashLength = 7

	// WorktreeIDHashLength is the number of hex characters used for worktree ID hash.
	WorktreeIDHashLength = 6
)

// HashWorktreeID returns a short hash of the worktree identifier.
// Used to create unique shadow branch names per worktree.
func HashWorktreeID(worktreeID string) string {
	h := sha256.Sum256([]byte(worktreeID))
	return hex.EncodeToString(h[:])[:WorktreeIDHashLength]
}

// WriteTemporary writes a temporary checkpoint to a shadow branch.
// Shadow branches are named entire/<base-commit-short-hash>.
// Returns the result containing commit hash and whether it was skipped.
// If the new tree hash matches the last checkpoint's tree hash, the checkpoint
// is skipped to avoid duplicate commits (deduplication).
func (s *GitStore) WriteTemporary(ctx context.Context, opts WriteTemporaryOptions) (WriteTemporaryResult, error) {
	_ = ctx // Reserved for future use (e.g., cancellation)

	// Validate base commit - required for shadow branch naming
	if opts.BaseCommit == "" {
		return WriteTemporaryResult{}, errors.New("BaseCommit is required for temporary checkpoint")
	}

	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(opts.SessionID); err != nil {
		return WriteTemporaryResult{}, fmt.Errorf("invalid temporary checkpoint options: %w", err)
	}

	// Get shadow branch name
	shadowBranchName := ShadowBranchNameForCommit(opts.BaseCommit, opts.WorktreeID)

	// Get or create shadow branch
	parentHash, baseTreeHash, err := s.getOrCreateShadowBranch(shadowBranchName)
	if err != nil {
		return WriteTemporaryResult{}, fmt.Errorf("failed to get shadow branch: %w", err)
	}

	// Get the last checkpoint's tree hash for deduplication
	var lastTreeHash plumbing.Hash
	if parentHash != plumbing.ZeroHash {
		if lastCommit, err := s.repo.CommitObject(parentHash); err == nil {
			lastTreeHash = lastCommit.TreeHash
		}
	}

	// Collect all files to include
	var allFiles []string
	if opts.IsFirstCheckpoint {
		// For the first checkpoint, capture all changed files (modified tracked + untracked)
		// using worktree.Status() which respects .gitignore and is much faster than filesystem walk.
		// The base tree from HEAD already contains all unchanged tracked files.
		allFiles, err = collectChangedFiles(s.repo)
		if err != nil {
			return WriteTemporaryResult{}, fmt.Errorf("failed to collect changed files: %w", err)
		}
	} else {
		// For subsequent checkpoints, only include modified/new files
		allFiles = make([]string, 0, len(opts.ModifiedFiles)+len(opts.NewFiles))
		allFiles = append(allFiles, opts.ModifiedFiles...)
		allFiles = append(allFiles, opts.NewFiles...)
	}

	// Build tree with changes
	treeHash, err := s.buildTreeWithChanges(baseTreeHash, allFiles, opts.DeletedFiles, opts.MetadataDir, opts.MetadataDirAbs)
	if err != nil {
		return WriteTemporaryResult{}, fmt.Errorf("failed to build tree: %w", err)
	}

	// Deduplication: skip if tree hash matches the last checkpoint
	if lastTreeHash != plumbing.ZeroHash && treeHash == lastTreeHash {
		return WriteTemporaryResult{
			CommitHash: parentHash,
			Skipped:    true,
		}, nil
	}

	// Create checkpoint commit with trailers
	commitMsg := trailers.FormatShadowCommit(opts.CommitMessage, opts.MetadataDir, opts.SessionID)

	commitHash, err := s.createCommit(treeHash, parentHash, commitMsg, opts.AuthorName, opts.AuthorEmail)
	if err != nil {
		return WriteTemporaryResult{}, fmt.Errorf("failed to create commit: %w", err)
	}

	// Update branch reference
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	newRef := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(newRef); err != nil {
		return WriteTemporaryResult{}, fmt.Errorf("failed to update branch reference: %w", err)
	}

	return WriteTemporaryResult{
		CommitHash: commitHash,
		Skipped:    false,
	}, nil
}

// ReadTemporary reads the latest checkpoint from a shadow branch.
// Returns nil if the shadow branch doesn't exist.
// worktreeID should be empty for main worktree or the internal git worktree name for linked worktrees.
func (s *GitStore) ReadTemporary(ctx context.Context, baseCommit, worktreeID string) (*ReadTemporaryResult, error) {
	_ = ctx // Reserved for future use

	shadowBranchName := ShadowBranchNameForCommit(baseCommit, worktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)

	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Branch not found is an expected case
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	// Extract session ID and metadata dir from commit trailers
	sessionID, _ := trailers.ParseSession(commit.Message)
	metadataDir, _ := trailers.ParseMetadata(commit.Message)

	return &ReadTemporaryResult{
		CommitHash:  ref.Hash(),
		TreeHash:    commit.TreeHash,
		SessionID:   sessionID,
		MetadataDir: metadataDir,
		Timestamp:   commit.Author.When,
	}, nil
}

// ListTemporary lists all shadow branches with their checkpoint info.
func (s *GitStore) ListTemporary(ctx context.Context) ([]TemporaryInfo, error) {
	_ = ctx // Reserved for future use

	iter, err := s.repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	var results []TemporaryInfo
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		branchName := ref.Name().Short()
		if !strings.HasPrefix(branchName, ShadowBranchPrefix) {
			return nil
		}

		// Skip the sessions branch
		if branchName == paths.MetadataBranchName {
			return nil
		}

		commit, commitErr := s.repo.CommitObject(ref.Hash())
		if commitErr != nil {
			//nolint:nilerr // Skip branches we can't read (non-fatal)
			return nil
		}

		sessionID, _ := trailers.ParseSession(commit.Message)

		// Extract base commit from branch name (handles new "entire/<commit>-<worktreeHash>" format)
		baseCommit, _, _ := ParseShadowBranchName(branchName)

		results = append(results, TemporaryInfo{
			BranchName:   branchName,
			BaseCommit:   baseCommit,
			LatestCommit: ref.Hash(),
			SessionID:    sessionID,
			Timestamp:    commit.Author.When,
		})

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to iterate branches: %w", err)
	}

	return results, nil
}

// WriteTemporaryTask writes a task checkpoint to a shadow branch.
// Task checkpoints include both code changes and task-specific metadata.
// Returns the commit hash of the created checkpoint.
func (s *GitStore) WriteTemporaryTask(ctx context.Context, opts WriteTemporaryTaskOptions) (plumbing.Hash, error) {
	_ = ctx // Reserved for future use

	// Validate base commit - required for shadow branch naming
	if opts.BaseCommit == "" {
		return plumbing.ZeroHash, errors.New("BaseCommit is required for task checkpoint")
	}

	// Validate identifiers to prevent path traversal and malformed data
	if err := validation.ValidateSessionID(opts.SessionID); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("invalid task checkpoint options: %w", err)
	}
	if err := validation.ValidateToolUseID(opts.ToolUseID); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("invalid task checkpoint options: %w", err)
	}
	if err := validation.ValidateAgentID(opts.AgentID); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("invalid task checkpoint options: %w", err)
	}

	// Get shadow branch name
	shadowBranchName := ShadowBranchNameForCommit(opts.BaseCommit, opts.WorktreeID)

	// Get or create shadow branch
	parentHash, baseTreeHash, err := s.getOrCreateShadowBranch(shadowBranchName)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get shadow branch: %w", err)
	}

	// Collect all files to include in the commit
	allFiles := make([]string, 0, len(opts.ModifiedFiles)+len(opts.NewFiles))
	allFiles = append(allFiles, opts.ModifiedFiles...)
	allFiles = append(allFiles, opts.NewFiles...)

	// Build new tree with code changes (no metadata dir yet)
	newTreeHash, err := s.buildTreeWithChanges(baseTreeHash, allFiles, opts.DeletedFiles, "", "")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to build tree: %w", err)
	}

	// Add task metadata to tree
	newTreeHash, err = s.addTaskMetadataToTree(newTreeHash, opts)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to add task metadata: %w", err)
	}

	// Create the commit
	commitHash, err := s.createCommit(newTreeHash, parentHash, opts.CommitMessage, opts.AuthorName, opts.AuthorEmail)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to create commit: %w", err)
	}

	// Update shadow branch reference
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to update shadow branch reference: %w", err)
	}

	return commitHash, nil
}

// addTaskMetadataToTree adds task checkpoint metadata to a git tree.
// When IsIncremental is true, only adds the incremental checkpoint file.
func (s *GitStore) addTaskMetadataToTree(baseTreeHash plumbing.Hash, opts WriteTemporaryTaskOptions) (plumbing.Hash, error) {
	// Get base tree and flatten it
	baseTree, err := s.repo.TreeObject(baseTreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get base tree: %w", err)
	}

	entries := make(map[string]object.TreeEntry)
	if err := FlattenTree(s.repo, baseTree, "", entries); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to flatten tree: %w", err)
	}

	// Compute metadata paths
	sessionMetadataDir := paths.EntireMetadataDir + "/" + opts.SessionID
	taskMetadataDir := sessionMetadataDir + "/tasks/" + opts.ToolUseID

	if opts.IsIncremental {
		// Incremental checkpoint: only add the checkpoint file
		// Use proper JSON marshaling to handle nil/empty IncrementalData correctly
		incrementalCheckpoint := struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Timestamp time.Time       `json:"timestamp"`
			Data      json.RawMessage `json:"data"`
		}{
			Type:      opts.IncrementalType,
			ToolUseID: opts.ToolUseID,
			Timestamp: time.Now().UTC(),
			Data:      opts.IncrementalData,
		}
		cpData, err := jsonutil.MarshalIndentWithNewline(incrementalCheckpoint, "", "  ")
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to marshal incremental checkpoint: %w", err)
		}

		cpBlobHash, err := CreateBlobFromContent(s.repo, cpData)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to create incremental checkpoint blob: %w", err)
		}
		cpFilename := fmt.Sprintf("%03d-%s.json", opts.IncrementalSequence, opts.ToolUseID)
		cpPath := taskMetadataDir + "/checkpoints/" + cpFilename
		entries[cpPath] = object.TreeEntry{
			Name: cpPath,
			Mode: filemode.Regular,
			Hash: cpBlobHash,
		}
	} else {
		// Final checkpoint: add transcripts and checkpoint.json

		// Add session transcript (with chunking support for large transcripts)
		if opts.TranscriptPath != "" {
			if transcriptContent, readErr := os.ReadFile(opts.TranscriptPath); readErr == nil {
				// Detect agent type from content for proper chunking
				agentType := agent.DetectAgentTypeFromContent(transcriptContent)

				// Chunk if necessary
				chunks, chunkErr := agent.ChunkTranscript(transcriptContent, agentType)
				if chunkErr != nil {
					logging.Warn(context.Background(), "failed to chunk transcript, checkpoint will be saved without transcript",
						slog.String("error", chunkErr.Error()),
						slog.String("session_id", opts.SessionID),
					)
				} else {
					for i, chunk := range chunks {
						chunkPath := sessionMetadataDir + "/" + agent.ChunkFileName(paths.TranscriptFileName, i)
						blobHash, blobErr := CreateBlobFromContent(s.repo, chunk)
						if blobErr != nil {
							logging.Warn(context.Background(), "failed to create blob for transcript chunk",
								slog.String("error", blobErr.Error()),
								slog.String("session_id", opts.SessionID),
								slog.Int("chunk_index", i),
							)
							continue
						}
						entries[chunkPath] = object.TreeEntry{
							Name: chunkPath,
							Mode: filemode.Regular,
							Hash: blobHash,
						}
					}
				}
			}
		}

		// Add subagent transcript if available
		if opts.SubagentTranscriptPath != "" && opts.AgentID != "" {
			if agentContent, readErr := os.ReadFile(opts.SubagentTranscriptPath); readErr == nil {
				if blobHash, blobErr := CreateBlobFromContent(s.repo, agentContent); blobErr == nil {
					agentPath := taskMetadataDir + "/agent-" + opts.AgentID + ".jsonl"
					entries[agentPath] = object.TreeEntry{
						Name: agentPath,
						Mode: filemode.Regular,
						Hash: blobHash,
					}
				}
			}
		}

		// Add checkpoint.json
		checkpointJSON := fmt.Sprintf(`{
  "session_id": %q,
  "tool_use_id": %q,
  "checkpoint_uuid": %q,
  "agent_id": %q
}`, opts.SessionID, opts.ToolUseID, opts.CheckpointUUID, opts.AgentID)

		blobHash, err := CreateBlobFromContent(s.repo, []byte(checkpointJSON))
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to create checkpoint blob: %w", err)
		}
		checkpointPath := taskMetadataDir + "/checkpoint.json"
		entries[checkpointPath] = object.TreeEntry{
			Name: checkpointPath,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	// Build new tree from entries
	return BuildTreeFromEntries(s.repo, entries)
}

// ListTemporaryCheckpoints lists all checkpoint commits on a shadow branch.
// This returns individual commits (rewind points), not just branch info.
// The sessionID filter, if provided, limits results to commits from that session.
// worktreeID should be empty for main worktree or the internal git worktree name for linked worktrees.
func (s *GitStore) ListTemporaryCheckpoints(ctx context.Context, baseCommit, worktreeID, sessionID string, limit int) ([]TemporaryCheckpointInfo, error) {
	shadowBranchName := ShadowBranchNameForCommit(baseCommit, worktreeID)
	return s.listCheckpointsForBranch(ctx, shadowBranchName, sessionID, limit)
}

// ListCheckpointsForBranch lists checkpoint commits for a shadow branch by name.
// Use this when you already have the full branch name (e.g., from ListTemporary).
// The sessionID filter, if provided, limits results to commits from that session.
func (s *GitStore) ListCheckpointsForBranch(ctx context.Context, branchName, sessionID string, limit int) ([]TemporaryCheckpointInfo, error) {
	return s.listCheckpointsForBranch(ctx, branchName, sessionID, limit)
}

// listCheckpointsForBranch lists checkpoint commits for a specific shadow branch name.
// This is an internal helper used by ListTemporaryCheckpoints, ListCheckpointsForBranch, and ListAllTemporaryCheckpoints.
func (s *GitStore) listCheckpointsForBranch(ctx context.Context, shadowBranchName, sessionID string, limit int) ([]TemporaryCheckpointInfo, error) {
	_ = ctx // Reserved for future use

	refName := plumbing.NewBranchReferenceName(shadowBranchName)

	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return nil, nil //nolint:nilerr // No shadow branch is expected case
	}

	iter, err := s.repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, fmt.Errorf("failed to get commit log: %w", err)
	}

	var results []TemporaryCheckpointInfo
	count := 0

	err = iter.ForEach(func(c *object.Commit) error {
		if count >= limit*5 { // Scan more to allow for session filtering
			return errStop
		}
		count++

		// Verify commit belongs to target session via Entire-Session trailer
		commitSessionID, hasTrailer := trailers.ParseSession(c.Message)
		if !hasTrailer {
			return nil // Skip commits without session trailer
		}
		if sessionID != "" && commitSessionID != sessionID {
			return nil // Skip commits from other sessions
		}

		// Get first line of message
		message := c.Message
		if idx := strings.Index(message, "\n"); idx > 0 {
			message = message[:idx]
		}

		info := TemporaryCheckpointInfo{
			CommitHash: c.Hash,
			Message:    message,
			SessionID:  commitSessionID,
			Timestamp:  c.Author.When,
		}

		// Check for task checkpoint first
		taskMetadataDir, foundTask := trailers.ParseTaskMetadata(c.Message)
		if foundTask {
			info.IsTaskCheckpoint = true
			info.MetadataDir = taskMetadataDir
			info.ToolUseID = extractToolUseIDFromPath(taskMetadataDir)
		} else {
			metadataDir, found := trailers.ParseMetadata(c.Message)
			if found {
				info.MetadataDir = metadataDir
			}
		}

		results = append(results, info)

		if len(results) >= limit {
			return errStop
		}
		return nil
	})

	if err != nil && !errors.Is(err, errStop) {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	return results, nil
}

// ListAllTemporaryCheckpoints lists checkpoint commits from ALL shadow branches.
// This is used for checkpoint lookup when the base commit is unknown (e.g., HEAD advanced since session start).
// The sessionID filter, if provided, limits results to commits from that session.
func (s *GitStore) ListAllTemporaryCheckpoints(ctx context.Context, sessionID string, limit int) ([]TemporaryCheckpointInfo, error) {
	_ = ctx // Reserved for future use

	// List all shadow branches
	branches, err := s.ListTemporary(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list shadow branches: %w", err)
	}

	var results []TemporaryCheckpointInfo

	// Iterate through each shadow branch and collect checkpoints
	for _, branch := range branches {
		// Use the branch name directly to get checkpoints
		branchCheckpoints, branchErr := s.listCheckpointsForBranch(ctx, branch.BranchName, sessionID, limit)
		if branchErr != nil {
			continue // Skip branches we can't read
		}
		results = append(results, branchCheckpoints...)
		if len(results) >= limit {
			results = results[:limit]
			break
		}
	}

	return results, nil
}

// extractToolUseIDFromPath extracts the ToolUseID from a task metadata directory path.
// Task metadata dirs have format: .entire/metadata/<session>/tasks/<toolUseID>
func extractToolUseIDFromPath(metadataDir string) string {
	parts := strings.Split(metadataDir, "/")
	if len(parts) >= 2 && parts[len(parts)-2] == "tasks" {
		return parts[len(parts)-1]
	}
	return ""
}

// errStop is a sentinel error used to break out of git log iteration.
var errStop = errors.New("stop iteration")

// GetTranscriptFromCommit retrieves the transcript from a specific commit's tree.
// This is used for shadow branch checkpoints where the transcript is stored in the commit tree
// rather than on the entire/sessions branch.
// commitHash is the commit to read from, metadataDir is the path within the tree.
// agentType is used for reassembling chunked transcripts in the correct format.
// Handles both chunked and non-chunked transcripts.
func (s *GitStore) GetTranscriptFromCommit(commitHash plumbing.Hash, metadataDir string, agentType agent.AgentType) ([]byte, error) {
	commit, err := s.repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	// Try to get the metadata subtree for chunk detection
	subTree, subTreeErr := tree.Tree(metadataDir)
	if subTreeErr == nil {
		// Use the helper function that handles chunking
		transcript, err := readTranscriptFromTree(subTree, agentType)
		if err == nil && transcript != nil {
			return transcript, nil
		}
	}

	// Fall back to direct file access (for backwards compatibility)
	transcriptPath := metadataDir + "/" + paths.TranscriptFileName
	if file, fileErr := tree.File(transcriptPath); fileErr == nil {
		content, contentErr := file.Contents()
		if contentErr == nil {
			return []byte(content), nil
		}
	}

	transcriptPath = metadataDir + "/" + paths.TranscriptFileNameLegacy
	if file, fileErr := tree.File(transcriptPath); fileErr == nil {
		content, contentErr := file.Contents()
		if contentErr == nil {
			return []byte(content), nil
		}
	}

	return nil, ErrNoTranscript
}

// ShadowBranchExists checks if a shadow branch exists for the given base commit and worktree.
// worktreeID should be empty for main worktree or the internal git worktree name for linked worktrees.
func (s *GitStore) ShadowBranchExists(baseCommit, worktreeID string) bool {
	shadowBranchName := ShadowBranchNameForCommit(baseCommit, worktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	_, err := s.repo.Reference(refName, true)
	return err == nil
}

// DeleteShadowBranch deletes the shadow branch for the given base commit and worktree.
// worktreeID should be empty for main worktree or the internal git worktree name for linked worktrees.
func (s *GitStore) DeleteShadowBranch(baseCommit, worktreeID string) error {
	shadowBranchName := ShadowBranchNameForCommit(baseCommit, worktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	if err := s.repo.Storer.RemoveReference(refName); err != nil {
		return fmt.Errorf("failed to delete shadow branch %s: %w", shadowBranchName, err)
	}
	return nil
}

// ShadowBranchNameForCommit returns the shadow branch name for a base commit hash
// and worktree identifier. The worktree ID should be empty for the main worktree
// or the internal git worktree name for linked worktrees.
// Format: entire/<commit[:7]>-<hash(worktreeID)[:6]>
func ShadowBranchNameForCommit(baseCommit, worktreeID string) string {
	commitPart := baseCommit
	if len(baseCommit) >= ShadowBranchHashLength {
		commitPart = baseCommit[:ShadowBranchHashLength]
	}
	worktreeHash := HashWorktreeID(worktreeID)
	return ShadowBranchPrefix + commitPart + "-" + worktreeHash
}

// ParseShadowBranchName extracts the commit prefix and worktree hash from a shadow branch name.
// Input format: "entire/<commit[:7]>-<worktreeHash[:6]>"
// Returns (commitPrefix, worktreeHash, ok). Returns ("", "", false) if not a valid shadow branch.
func ParseShadowBranchName(branchName string) (commitPrefix, worktreeHash string, ok bool) {
	if !strings.HasPrefix(branchName, ShadowBranchPrefix) {
		return "", "", false
	}
	suffix := strings.TrimPrefix(branchName, ShadowBranchPrefix)

	// Find the last dash - everything before is commit prefix, after is worktree hash
	lastDash := strings.LastIndex(suffix, "-")
	if lastDash == -1 || lastDash == 0 || lastDash == len(suffix)-1 {
		// No dash, or dash at start/end - invalid format
		// Could be old format "entire/<commit[:7]>" without worktree hash
		return suffix, "", true // Return as commit prefix with empty worktree hash
	}

	return suffix[:lastDash], suffix[lastDash+1:], true
}

// getOrCreateShadowBranch gets or creates the shadow branch for checkpoints.
// Returns (parentHash, baseTreeHash, error).
func (s *GitStore) getOrCreateShadowBranch(branchName string) (plumbing.Hash, plumbing.Hash, error) {
	refName := plumbing.NewBranchReferenceName(branchName)
	ref, err := s.repo.Reference(refName, true)

	if err == nil {
		// Branch exists
		commit, err := s.repo.CommitObject(ref.Hash())
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get commit object: %w", err)
		}
		return ref.Hash(), commit.TreeHash, nil
	}

	// Branch doesn't exist, use current HEAD's tree as base
	head, err := s.repo.Head()
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get HEAD: %w", err)
	}

	headCommit, err := s.repo.CommitObject(head.Hash())
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get HEAD commit: %w", err)
	}

	return plumbing.ZeroHash, headCommit.TreeHash, nil
}

// buildTreeWithChanges builds a git tree with the given changes.
// metadataDir is the relative path for git tree entries, metadataDirAbs is the absolute path
// for filesystem operations (needed when CLI is run from a subdirectory).
func (s *GitStore) buildTreeWithChanges(
	baseTreeHash plumbing.Hash,
	modifiedFiles, deletedFiles []string,
	metadataDir, metadataDirAbs string,
) (plumbing.Hash, error) {
	// Get repo root for resolving file paths
	// This is critical because fileExists() and createBlobFromFile() use os.Stat()
	// which resolves relative to CWD. The modifiedFiles are repo-relative paths,
	// so we must resolve them against repo root, not CWD.
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get repo root: %w", err)
	}

	// Get the base tree
	baseTree, err := s.repo.TreeObject(baseTreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get base tree: %w", err)
	}

	// Flatten existing tree
	entries := make(map[string]object.TreeEntry)
	if err := FlattenTree(s.repo, baseTree, "", entries); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to flatten base tree: %w", err)
	}

	// Remove deleted files
	for _, file := range deletedFiles {
		delete(entries, file)
	}

	// Add/update modified files
	for _, file := range modifiedFiles {
		// Resolve path relative to repo root for filesystem operations
		absPath := filepath.Join(repoRoot, file)
		if !fileExists(absPath) {
			delete(entries, file)
			continue
		}

		blobHash, mode, err := createBlobFromFile(s.repo, absPath)
		if err != nil {
			// Skip files that can't be staged (may have been deleted since detection)
			continue
		}

		entries[file] = object.TreeEntry{
			Name: file,
			Mode: mode,
			Hash: blobHash,
		}
	}

	// Add metadata directory files
	if metadataDir != "" && metadataDirAbs != "" {
		if err := addDirectoryToEntriesWithAbsPath(s.repo, metadataDirAbs, metadataDir, entries); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to add metadata directory: %w", err)
		}
	}

	// Build tree
	return BuildTreeFromEntries(s.repo, entries)
}

// createCommit creates a commit object.
func (s *GitStore) createCommit(treeHash, parentHash plumbing.Hash, message, authorName, authorEmail string) (plumbing.Hash, error) {
	now := time.Now()
	sig := object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  now,
	}

	commit := &object.Commit{
		TreeHash:  treeHash,
		Author:    sig,
		Committer: sig,
		Message:   message,
	}

	// Add parent if not a new branch
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}

	obj := s.repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit: %w", err)
	}

	hash, err := s.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store commit: %w", err)
	}

	return hash, nil
}

// Helper functions extracted from strategy/common.go
// These are exported for use by strategy package (push_common.go, session_test.go)

// FlattenTree recursively flattens a tree into a map of full paths to entries.
func FlattenTree(repo *git.Repository, tree *object.Tree, prefix string, entries map[string]object.TreeEntry) error {
	for _, entry := range tree.Entries {
		fullPath := entry.Name
		if prefix != "" {
			fullPath = prefix + "/" + entry.Name
		}

		if entry.Mode == filemode.Dir {
			// Recurse into subtree
			subtree, err := repo.TreeObject(entry.Hash)
			if err != nil {
				return fmt.Errorf("failed to get subtree %s: %w", fullPath, err)
			}
			if err := FlattenTree(repo, subtree, fullPath, entries); err != nil {
				return err
			}
		} else {
			entries[fullPath] = object.TreeEntry{
				Name: fullPath,
				Mode: entry.Mode,
				Hash: entry.Hash,
			}
		}
	}
	return nil
}

// fileExists checks if a file exists at the given path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// createBlobFromFile creates a blob object from a file in the working directory.
func createBlobFromFile(repo *git.Repository, filePath string) (plumbing.Hash, filemode.FileMode, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to stat file: %w", err)
	}

	// Determine file mode
	mode := filemode.Regular
	if info.Mode()&0o111 != 0 {
		mode = filemode.Executable
	}
	if info.Mode()&os.ModeSymlink != 0 {
		mode = filemode.Symlink
	}

	// Read file contents
	content, err := os.ReadFile(filePath) //nolint:gosec // filePath comes from walking the repository
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to read file: %w", err)
	}

	// Create blob object
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))

	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to get object writer: %w", err)
	}

	_, err = writer.Write(content)
	if err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to write blob content: %w", err)
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to close blob writer: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to store blob object: %w", err)
	}

	return hash, mode, nil
}

// addDirectoryToEntriesWithAbsPath recursively adds all files in a directory to the entries map.
func addDirectoryToEntriesWithAbsPath(repo *git.Repository, dirPathAbs, dirPathRel string, entries map[string]object.TreeEntry) error {
	err := filepath.Walk(dirPathAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// Calculate relative path within the directory, then join with dirPathRel for tree entry
		relWithinDir, err := filepath.Rel(dirPathAbs, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		blobHash, mode, err := createBlobFromFile(repo, path)
		if err != nil {
			return fmt.Errorf("failed to create blob for %s: %w", path, err)
		}

		treePath := filepath.Join(dirPathRel, relWithinDir)
		entries[treePath] = object.TreeEntry{
			Name: treePath,
			Mode: mode,
			Hash: blobHash,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk directory %s: %w", dirPathAbs, err)
	}
	return nil
}

// treeNode represents a node in our tree structure.
type treeNode struct {
	entries map[string]*treeNode // subdirectories
	files   []object.TreeEntry   // files in this directory
}

// BuildTreeFromEntries builds a proper git tree structure from flattened file entries.
// Exported for use by strategy package (push_common.go, session_test.go)
func BuildTreeFromEntries(repo *git.Repository, entries map[string]object.TreeEntry) (plumbing.Hash, error) {
	// Build a tree structure
	root := &treeNode{
		entries: make(map[string]*treeNode),
		files:   []object.TreeEntry{},
	}

	// Insert all entries into the tree structure
	for fullPath, entry := range entries {
		parts := strings.Split(fullPath, "/")
		insertIntoTree(root, parts, entry)
	}

	// Recursively build tree objects from bottom up
	return buildTreeObject(repo, root)
}

// insertIntoTree inserts a file entry into the tree structure.
func insertIntoTree(node *treeNode, pathParts []string, entry object.TreeEntry) {
	if len(pathParts) == 1 {
		// This is a file in the current directory
		node.files = append(node.files, object.TreeEntry{
			Name: pathParts[0],
			Mode: entry.Mode,
			Hash: entry.Hash,
		})
		return
	}

	// This is in a subdirectory
	dirName := pathParts[0]
	if node.entries[dirName] == nil {
		node.entries[dirName] = &treeNode{
			entries: make(map[string]*treeNode),
			files:   []object.TreeEntry{},
		}
	}
	insertIntoTree(node.entries[dirName], pathParts[1:], entry)
}

// buildTreeObject recursively builds tree objects from a treeNode.
func buildTreeObject(repo *git.Repository, node *treeNode) (plumbing.Hash, error) {
	var treeEntries []object.TreeEntry

	// Add files
	treeEntries = append(treeEntries, node.files...)

	// Recursively build subtrees
	for name, subnode := range node.entries {
		subHash, err := buildTreeObject(repo, subnode)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		treeEntries = append(treeEntries, object.TreeEntry{
			Name: name,
			Mode: filemode.Dir,
			Hash: subHash,
		})
	}

	// Sort entries (git requires sorted entries)
	sortTreeEntries(treeEntries)

	// Create tree object
	tree := &object.Tree{Entries: treeEntries}

	obj := repo.Storer.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode tree: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store tree: %w", err)
	}

	return hash, nil
}

// sortTreeEntries sorts tree entries in git's required order.
// Git sorts tree entries by name, with directories having a trailing /
func sortTreeEntries(entries []object.TreeEntry) {
	sort.Slice(entries, func(i, j int) bool {
		nameI := entries[i].Name
		nameJ := entries[j].Name
		if entries[i].Mode == filemode.Dir {
			nameI += "/"
		}
		if entries[j].Mode == filemode.Dir {
			nameJ += "/"
		}
		return nameI < nameJ
	})
}

// collectChangedFiles collects all changed files (modified tracked + untracked non-ignored)
// using worktree.Status(). This is much faster than filesystem walk because it respects
// .gitignore and only returns files that git knows about.
//
// For the first checkpoint, we need to capture:
// - Modified tracked files (user's uncommitted changes)
// - Untracked non-ignored files (new files not yet added to git)
//
// The base tree from HEAD already contains all unchanged tracked files.
func collectChangedFiles(repo *git.Repository) ([]string, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("failed to get worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, fmt.Errorf("failed to get worktree status: %w", err)
	}

	var files []string
	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Include modified tracked files and untracked files
		// worktree.Status() respects .gitignore, so untracked files here are not ignored
		//nolint:exhaustive // default case handles other statuses (Unmodified, Added, Renamed, etc.)
		switch st.Worktree {
		case git.Modified, git.Untracked:
			files = append(files, file)
		case git.Deleted:
			// Deleted files are handled separately via DeletedFiles parameter
			continue
		default:
			// Other statuses are either handled by staging check below or not relevant
		}

		// Also check staging area for modifications
		// A file might be staged (Modified in Staging) but unchanged in worktree
		if st.Staging == git.Modified || st.Staging == git.Added {
			// Avoid duplicates - only add if not already in list
			if !slices.Contains(files, file) {
				files = append(files, file)
			}
		}
	}

	return files, nil
}
