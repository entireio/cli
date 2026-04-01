package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Common branch name constants for default branch detection.
const (
	branchMain   = "main"
	branchMaster = "master"
	// Strategy name constants
	StrategyNameManualCommit = "manual-commit"
)

// MaxCommitTraversalDepth is the safety limit for walking git commit history.
// Prevents unbounded traversal in repositories with very long histories.
const MaxCommitTraversalDepth = 1000

// errStop is a sentinel error used to break out of git log iteration.
// Shared across strategies that iterate through git commits.
// NOTE: A similar sentinel exists in checkpoint/temporary.go - this is intentional.
// Each package needs its own package-scoped sentinel for git log iteration patterns.
var errStop = errors.New("stop iteration")

// IsEmptyRepository returns true if the repository has no commits yet.
// After git-init, HEAD points to an unborn branch (e.g., refs/heads/main)
// whose target does not yet exist. repo.Head() returns ErrReferenceNotFound
// in this case.
func IsEmptyRepository(repo *git.Repository) bool {
	_, err := repo.Head()
	return errors.Is(err, plumbing.ErrReferenceNotFound)
}

// EnsureSetup ensures the strategy is properly set up.
func EnsureSetup(ctx context.Context) error {
	if err := EnsureEntireGitignore(ctx); err != nil {
		return err
	}

	// Ensure the entire/checkpoints/v1 orphan branch exists for permanent session storage
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	if err := EnsureMetadataBranch(repo); err != nil {
		return fmt.Errorf("failed to ensure metadata branch: %w", err)
	}

	// Install generic hooks (they delegate to strategy at runtime)
	if !IsGitHookInstalled(ctx) {
		localDev, absoluteHookPath := hookSettingsFromConfig(ctx)
		if _, err := InstallGitHook(ctx, true, localDev, absoluteHookPath); err != nil {
			return fmt.Errorf("failed to install git hooks: %w", err)
		}
	}
	return nil
}

// IsAncestorOf checks if commit is an ancestor of (or equal to) target.
// Returns true if target can reach commit by following parent links.
// Limits search to MaxCommitTraversalDepth commits to avoid excessive traversal.
func IsAncestorOf(ctx context.Context, repo *git.Repository, commit, target plumbing.Hash) bool {
	if commit == target {
		return true
	}

	iter, err := repo.Log(&git.LogOptions{From: target})
	if err != nil {
		return false
	}
	defer iter.Close()

	found := false
	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are non-fatal
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		count++
		if count > MaxCommitTraversalDepth {
			return errStop
		}
		if c.Hash == commit {
			found = true
			return errStop
		}
		return nil
	})

	return found
}

// ListCheckpoints returns all checkpoints from the entire/checkpoints/v1 branch.
// Scans sharded paths: <id[:2]>/<id[2:]>/ directories containing metadata.json.
func ListCheckpoints(ctx context.Context) ([]CheckpointInfo, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Warn (once per process) if metadata branches are disconnected
	WarnIfMetadataDisconnected()

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		//nolint:nilerr // No sessions branch yet is expected, return empty list
		return []CheckpointInfo{}, nil
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	var checkpoints []CheckpointInfo

	// Scan sharded structure: <2-char-prefix>/<remaining-id>/metadata.json
	// The tree has 2-character directories (hex buckets)
	for _, bucketEntry := range tree.Entries {
		if bucketEntry.Mode != filemode.Dir {
			continue
		}
		// Bucket should be 2 hex chars
		if len(bucketEntry.Name) != 2 {
			continue
		}

		bucketTree, treeErr := repo.TreeObject(bucketEntry.Hash)
		if treeErr != nil {
			continue
		}

		// Each entry in the bucket is the remaining part of the checkpoint ID
		for _, checkpointEntry := range bucketTree.Entries {
			if checkpointEntry.Mode != filemode.Dir {
				continue
			}

			checkpointTree, cpTreeErr := repo.TreeObject(checkpointEntry.Hash)
			if cpTreeErr != nil {
				continue
			}

			// Reconstruct checkpoint ID: <bucket><remaining>
			checkpointIDStr := bucketEntry.Name + checkpointEntry.Name
			checkpointID, cpErr := id.NewCheckpointID(checkpointIDStr)
			if cpErr != nil {
				// Skip invalid checkpoint IDs
				continue
			}

			info := CheckpointInfo{
				CheckpointID: checkpointID,
			}

			// Get details from metadata file (CheckpointSummary format)
			if summary, ok := decodeSummaryLiteFromTree(checkpointTree); ok {
				info.CheckpointsCount = summary.CheckpointsCount
				info.FilesTouched = summary.FilesTouched
				info.SessionCount = len(summary.Sessions)

				// Read session-level metadata for Agent, SessionID, CreatedAt, SessionIDs
				for i, sessionPaths := range summary.Sessions {
					if sessionPaths.Metadata == "" {
						continue
					}
					// SessionFilePaths contains absolute paths with leading "/"
					// Strip the leading "/" for tree.File() which expects paths without leading slash
					sessionMetadataPath := strings.TrimPrefix(sessionPaths.Metadata, "/")
					sessionMeta, sErr := decodeSessionMetadataLite(tree, sessionMetadataPath)
					if sErr != nil {
						continue
					}
					info.SessionIDs = append(info.SessionIDs, sessionMeta.SessionID)
					// Use first session's metadata for Agent, SessionID, CreatedAt
					if i == 0 {
						info.Agent = sessionMeta.Agent
						info.SessionID = sessionMeta.SessionID
						info.CreatedAt = sessionMeta.CreatedAt
						info.IsTask = sessionMeta.IsTask
						info.ToolUseID = sessionMeta.ToolUseID
					}
				}
			}

			checkpoints = append(checkpoints, info)
		}
	}

	// Sort by time (most recent first)
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})

	return checkpoints, nil
}

const (
	entireGitignore    = ".entire/.gitignore"
	entireDir          = ".entire"
	gitDir             = ".git"
	shadowBranchPrefix = "entire/"
)

// isProtectedPath returns true if relPath is inside a directory that should
// never be modified or deleted during rewind or other destructive operations.
// Protected directories include git internals, entire metadata, and all
// registered agent config directories.
func isProtectedPath(relPath string) bool {
	for _, dir := range protectedDirs() {
		if paths.IsSubpath(dir, relPath) {
			return true
		}
	}
	return false
}

// protectedDirs returns the list of directories to protect. This combines
// static infrastructure dirs with agent-reported dirs from the registry.
// The result is cached via sync.Once since it's called per-file when filtering untracked files.
//
// NOTE: The cache is never invalidated. In production this is fine (the agent registry
// is populated at init time and never changes). However, tests that mutate the agent
// registry after the first call to protectedDirs/isProtectedPath will see stale results.
// If you need to test isProtectedPath with a custom registry, either:
//   - run those tests in a separate process, or
//   - call resetProtectedDirsForTest() to clear the cache.
func protectedDirs() []string {
	protectedDirsOnce.Do(func() {
		protectedDirsCache = append([]string{gitDir, entireDir}, agent.AllProtectedDirs()...)
	})
	return protectedDirsCache
}

var (
	protectedDirsOnce  sync.Once
	protectedDirsCache []string
)

var initRedactionOnce sync.Once

// EnsureRedactionConfigured loads PII redaction settings and configures the
// redact package. No-op if PII is not enabled in settings.
// Must be called at each process entry point before checkpoint writes
// (e.g., hook PersistentPreRunE, doctor PreRun).
func EnsureRedactionConfigured() {
	initRedactionOnce.Do(func() {
		ctx := context.Background()
		s, err := settings.Load(ctx)
		if err != nil {
			logCtx := logging.WithComponent(ctx, "redaction")
			logging.Warn(logCtx, "failed to load settings for PII redaction", slog.String("error", err.Error()))
			return
		}
		if s.Redaction == nil || s.Redaction.PII == nil || !s.Redaction.PII.Enabled {
			return
		}
		pii := s.Redaction.PII
		cfg := redact.PIIConfig{
			Enabled:        true,
			Categories:     make(map[redact.PIICategory]bool),
			CustomPatterns: pii.CustomPatterns,
		}
		// Email and phone default to true when PII is enabled; address defaults to false.
		cfg.Categories[redact.PIIEmail] = pii.Email == nil || *pii.Email
		cfg.Categories[redact.PIIPhone] = pii.Phone == nil || *pii.Phone
		cfg.Categories[redact.PIIAddress] = pii.Address != nil && *pii.Address
		redact.ConfigurePII(cfg)
	})
}

// resolveAgentType picks the best agent type from the context and existing state.
// Priority: existing state > context value.
func resolveAgentType(ctxAgentType types.AgentType, state *SessionState) types.AgentType {
	if state != nil && state.AgentType != "" {
		return state.AgentType
	}
	return ctxAgentType
}

// EnsureMetadataBranch creates or updates the local entire/checkpoints/v1 branch.
// If the remote-tracking branch (origin/entire/checkpoints/v1) exists and the local
// branch is missing or empty, creates/updates the local branch from it.
// Otherwise creates an empty orphan.
func EnsureMetadataBranch(repo *git.Repository) error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)

	// Check if remote-tracking branch exists (e.g., after clone/fetch)
	remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	remoteRef, remoteErr := repo.Reference(remoteRefName, true)
	if remoteErr != nil && !errors.Is(remoteErr, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("failed to check remote metadata branch: %w", remoteErr)
	}

	// Check if local branch already exists
	localRef, err := repo.Reference(refName, true)
	if err == nil {
		if remoteErr == nil && localRef.Hash() != remoteRef.Hash() {
			// Local and remote exist but differ — determine relationship
			isEmpty, checkErr := isEmptyMetadataBranch(repo, localRef)
			if checkErr != nil {
				return fmt.Errorf("failed to check metadata branch contents: %w", checkErr)
			}
			if isEmpty {
				// Empty orphan — just point to remote
				ref := plumbing.NewHashReference(refName, remoteRef.Hash())
				if setErr := repo.Storer.SetReference(ref); setErr != nil {
					return fmt.Errorf("failed to update metadata branch from remote: %w", setErr)
				}
				fmt.Fprintf(os.Stderr, "[entire] Updated local branch '%s' from origin\n", paths.MetadataBranchName)
			} else {
				// Local has real data and differs from remote — if disconnected
				// (no common ancestor), reconciliation happens at pre-push time
				// or via 'entire doctor'. Read paths warn but do not auto-fix.
				logging.Debug(context.Background(), "metadata branch differs from remote, reconciliation deferred to read/write time",
					"local_hash", localRef.Hash().String()[:7],
					"remote_hash", remoteRef.Hash().String()[:7],
				)
			}
		}
		return nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("failed to check metadata branch: %w", err)
	}

	// Local branch doesn't exist — create from remote if available
	if remoteErr == nil {
		ref := plumbing.NewHashReference(refName, remoteRef.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return fmt.Errorf("failed to create metadata branch from remote: %w", err)
		}
		fmt.Fprintf(os.Stderr, "✓ Created local branch '%s' from origin\n", paths.MetadataBranchName)
		return nil
	}

	// No local or remote branch — create empty orphan
	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	obj := repo.Storer.NewEncodedObject()
	if err := emptyTree.Encode(obj); err != nil {
		return fmt.Errorf("failed to encode empty tree: %w", err)
	}
	emptyTreeHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return fmt.Errorf("failed to store empty tree: %w", err)
	}

	// Create orphan commit (no parent)
	now := time.Now()
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	sig := object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  now,
	}

	commit := &object.Commit{
		TreeHash:  emptyTreeHash,
		Author:    sig,
		Committer: sig,
		Message:   "Initialize metadata branch\n\nThis branch stores session metadata.\n",
	}
	// Note: No ParentHashes - this is an orphan commit

	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return fmt.Errorf("failed to encode orphan commit: %w", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		return fmt.Errorf("failed to store orphan commit: %w", err)
	}

	// Create branch reference
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to create metadata branch: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✓ Created orphan branch '%s' for session metadata\n", paths.MetadataBranchName)
	return nil
}

// isEmptyMetadataBranch returns true if the branch ref points to a commit with an empty tree.
// Only checks the tip commit — if a data commit sits on top of an empty orphan, this returns
// false, which is correct: the bug this detects creates a single empty orphan as the tip.
func isEmptyMetadataBranch(repo *git.Repository, ref *plumbing.Reference) (bool, error) {
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return false, fmt.Errorf("failed to get commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("failed to get tree: %w", err)
	}
	return len(tree.Entries) == 0, nil
}

// sessionMetadataLite contains only the fields needed from session-level metadata.json.
// Using a minimal struct avoids allocating large nested objects (Summary, InitialAttribution,
// TokenUsage, etc.) that CommittedMetadata carries but callers never need here.
type sessionMetadataLite struct {
	SessionID string          `json:"session_id"`
	Agent     types.AgentType `json:"agent,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	IsTask    bool            `json:"is_task,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

// checkpointSummaryLite contains only the fields needed from the root metadata.json.
// Avoids allocating TokenUsage and other heavy fields from CheckpointSummary.
type checkpointSummaryLite struct {
	CheckpointID     id.CheckpointID               `json:"checkpoint_id"`
	CheckpointsCount int                           `json:"checkpoints_count"`
	FilesTouched     []string                      `json:"files_touched"`
	Sessions         []checkpoint.SessionFilePaths `json:"sessions"`
}

// decodeSessionMetadataLite reads a session metadata.json from the tree using a streaming
// json.Decoder and a minimal struct to avoid allocating large unused fields.
func decodeSessionMetadataLite(tree checkpoint.FileReader, metadataPath string) (*sessionMetadataLite, error) {
	file, err := tree.File(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("session metadata file %s: %w", metadataPath, err)
	}
	reader, err := file.Reader()
	if err != nil {
		return nil, fmt.Errorf("session metadata reader %s: %w", metadataPath, err)
	}
	defer reader.Close()

	var meta sessionMetadataLite
	if err := json.NewDecoder(reader).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode session metadata %s: %w", metadataPath, err)
	}
	return &meta, nil
}

// decodeSummaryLiteFromTree reads and decodes metadata.json from a checkpoint tree
// using a streaming decoder and minimal struct. Returns the decoded summary and true
// if successful with at least one session, or zero value and false otherwise.
func decodeSummaryLiteFromTree(checkpointTree checkpoint.FileReader) (checkpointSummaryLite, bool) {
	metadataFile, fileErr := checkpointTree.File(paths.MetadataFileName)
	if fileErr != nil {
		return checkpointSummaryLite{}, false
	}
	reader, readerErr := metadataFile.Reader()
	if readerErr != nil {
		return checkpointSummaryLite{}, false
	}
	defer reader.Close()

	var summary checkpointSummaryLite
	if err := json.NewDecoder(reader).Decode(&summary); err != nil || len(summary.Sessions) == 0 {
		return checkpointSummaryLite{}, false
	}
	return summary, true
}

// ReadCheckpointMetadata reads metadata.json from a checkpoint path on entire/checkpoints/v1.
// With the new format, root metadata.json is a CheckpointSummary with Agents array.
// This function reads the summary and extracts relevant fields into CheckpointInfo,
// also reading session-level metadata for IsTask/ToolUseID fields.
//
// Uses streaming json.Decoder and minimal structs to avoid loading large nested
// objects (Summary, InitialAttribution, TokenUsage) into memory.
func ReadCheckpointMetadata(tree checkpoint.FileReader, checkpointPath string) (*CheckpointInfo, error) {
	metadataPath := checkpointPath + "/metadata.json"
	file, err := tree.File(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find metadata at %s: %w", metadataPath, err)
	}

	// Session metadata paths in the summary are absolute (e.g., "/ca/b75de47439/0/metadata.json").
	// For a full tree, strip the leading "/" to get tree-relative paths.
	normalizePath := func(raw string) string {
		return strings.TrimPrefix(raw, "/")
	}
	return decodeCheckpointInfo(file, tree, checkpointPath, normalizePath)
}

// ReadCheckpointMetadataFromSubtree reads checkpoint metadata from a tree that is
// already rooted at the checkpoint directory (e.g., after tree.Tree(checkpointID.Path())).
// checkpointPath is the original sharded path (e.g., "ca/b75de47439") and is used
// to strip the prefix from absolute session metadata paths stored in the summary.
func ReadCheckpointMetadataFromSubtree(tree checkpoint.FileReader, checkpointPath string) (*CheckpointInfo, error) {
	file, err := tree.File(paths.MetadataFileName)
	if err != nil {
		return nil, fmt.Errorf("failed to find %s in checkpoint subtree: %w", paths.MetadataFileName, err)
	}

	// Session metadata paths are absolute from the tree root (e.g., "/ca/b75de47439/0/metadata.json").
	// Strip the checkpoint prefix to get paths relative to the subtree (e.g., "0/metadata.json").
	prefix := "/" + checkpointPath + "/"
	normalizePath := func(raw string) string {
		return strings.TrimPrefix(raw, prefix)
	}
	return decodeCheckpointInfo(file, tree, checkpointPath, normalizePath)
}

// decodeCheckpointInfo is the shared implementation for ReadCheckpointMetadata and
// ReadCheckpointMetadataFromSubtree. It decodes the root metadata.json, reads
// per-session metadata, and populates a CheckpointInfo.
//
// normalizePath transforms absolute session metadata paths from the summary into
// paths that are valid for tree.File() lookups (the transform differs depending on
// whether tree is a full metadata branch tree or a checkpoint subtree).
func decodeCheckpointInfo(
	file checkpoint.FileOpener,
	tree checkpoint.FileReader,
	checkpointPath string,
	normalizePath func(string) string,
) (*CheckpointInfo, error) {
	reader, err := file.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata: %w", err)
	}
	defer reader.Close()

	// Try to parse as CheckpointSummary first (new format) using lite struct
	var summary checkpointSummaryLite
	if decodeErr := json.NewDecoder(reader).Decode(&summary); decodeErr == nil {
		if len(summary.Sessions) > 0 {
			info := &CheckpointInfo{
				CheckpointID:     summary.CheckpointID,
				CheckpointsCount: summary.CheckpointsCount,
				FilesTouched:     summary.FilesTouched,
				SessionCount:     len(summary.Sessions),
			}

			// Read all sessions' metadata to populate SessionIDs and get other fields from first session
			var sessionIDs []string
			for i, sessionPaths := range summary.Sessions {
				if sessionPaths.Metadata == "" {
					continue
				}
				sessionMetadataPath := normalizePath(sessionPaths.Metadata)
				sessionMeta, sErr := decodeSessionMetadataLite(tree, sessionMetadataPath)
				if sErr != nil {
					logging.Debug(context.Background(), "decodeCheckpointInfo: session metadata decode failed",
						slog.Int("session_index", i),
						slog.String("metadata_path", sessionMetadataPath),
						slog.String("checkpoint_path", checkpointPath),
						slog.String("error", sErr.Error()),
					)
					continue
				}
				sessionIDs = append(sessionIDs, sessionMeta.SessionID)
				if i == 0 {
					info.Agent = sessionMeta.Agent
					info.SessionID = sessionMeta.SessionID
					info.CreatedAt = sessionMeta.CreatedAt
					info.IsTask = sessionMeta.IsTask
					info.ToolUseID = sessionMeta.ToolUseID
				}
			}
			info.SessionIDs = sessionIDs
			return info, nil
		}
	}

	// Fall back to parsing as CheckpointInfo (old format or direct info).
	// Re-read the file since the decoder consumed the reader.
	fallbackReader, err := file.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to re-read metadata: %w", err)
	}
	defer fallbackReader.Close()

	var metadata CheckpointInfo
	if err := json.NewDecoder(fallbackReader).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}
	return &metadata, nil
}

// GetMetadataBranchTree returns the tree object for the entire/checkpoints/v1 branch.
func GetMetadataBranchTree(repo *git.Repository) (*object.Tree, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch reference: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch tree: %w", err)
	}
	return tree, nil
}

// ExtractFirstPrompt extracts and truncates the first meaningful prompt from prompt content.
// Prompts are separated by "\n\n---\n\n". Skips empty prompts and separator-only content.
// Returns empty string if no valid prompt is found.
func ExtractFirstPrompt(content string) string {
	if content == "" {
		return ""
	}

	// Prompts are separated by "\n\n---\n\n"
	// Find the first non-empty prompt
	prompts := strings.Split(content, "\n\n---\n\n")
	var firstPrompt string
	for _, p := range prompts {
		cleaned := strings.TrimSpace(p)
		// Skip empty prompts or prompts that are just dashes/separators
		if cleaned == "" || isOnlySeparators(cleaned) {
			continue
		}
		firstPrompt = cleaned
		break
	}

	if firstPrompt == "" {
		return ""
	}

	return TruncateDescription(firstPrompt, MaxDescriptionLength)
}

// ReadSessionPromptFromTree reads the first meaningful prompt from a checkpoint's prompt.txt file in a git tree.
// Returns an empty string if the prompt cannot be read.
func ReadSessionPromptFromTree(tree *object.Tree, checkpointPath string) string {
	promptPath := checkpointPath + "/" + paths.PromptFileName
	file, err := tree.File(promptPath)
	if err != nil {
		return ""
	}

	content, err := file.Contents()
	if err != nil {
		return ""
	}

	return ExtractFirstPrompt(content)
}

// ReadAgentTypeFromTree reads the agent type from a checkpoint's metadata.json file in a git tree.
// If metadata.json doesn't exist (shadow branches), it falls back to detecting the agent
// from the presence of agent-specific config files (.gemini/settings.json or .claude/).
// Returns agent.AgentTypeUnknown if the agent type cannot be determined.
func ReadAgentTypeFromTree(tree *object.Tree, checkpointPath string) types.AgentType {
	// First, try to read from metadata.json (present in condensed/committed checkpoints)
	metadataPath := checkpointPath + "/" + paths.MetadataFileName
	if file, err := tree.File(metadataPath); err == nil {
		if content, err := file.Contents(); err == nil {
			var metadata checkpoint.CommittedMetadata
			if err := json.Unmarshal([]byte(content), &metadata); err == nil && metadata.Agent != "" {
				return metadata.Agent
			}
		}
	}

	// Fall back to detecting agent from config files (shadow branches don't have metadata.json).
	// Order: Gemini (most specific check), Claude (established default), OpenCode (newest/preview).
	if _, err := tree.File(".gemini/settings.json"); err == nil {
		return agent.AgentTypeGemini
	}
	if _, err := tree.Tree(".claude"); err == nil {
		return agent.AgentTypeClaudeCode
	}
	// OpenCode: .opencode directory or opencode.json config
	if _, err := tree.Tree(".opencode"); err == nil {
		return agent.AgentTypeOpenCode
	}
	if _, err := tree.File("opencode.json"); err == nil {
		return agent.AgentTypeOpenCode
	}

	return agent.AgentTypeUnknown
}

// isOnlySeparators checks if a string contains only dashes, spaces, and newlines.
func isOnlySeparators(s string) bool {
	for _, r := range s {
		if r != '-' && r != ' ' && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

// ReadLatestSessionPromptFromCommittedTree reads the first prompt from a committed checkpoint's
// latest session on the metadata branch tree. This navigates the sharded directory layout:
// <cpID.Path()>/<latestSessionIndex>/prompt.txt
//
// Falls back through earlier sessions when the latest has no prompt.
// Avoids reading full transcripts — only reads prompt.txt files.
// sessionCount is the number of sessions in the checkpoint (from CommittedInfo.SessionCount).
func ReadLatestSessionPromptFromCommittedTree(tree *object.Tree, cpID id.CheckpointID, sessionCount int) string {
	cpPath := cpID.Path()
	cpTree, err := tree.Tree(cpPath)
	if err != nil {
		return ""
	}

	// Find the latest session subdirectory with a prompt.
	// Sessions use 0-based indexing: 0/, 1/, 2/, etc.
	// Start from the latest and fall back through earlier sessions
	// when the latest has no prompt (e.g. a test or empty session was
	// condensed alongside a real one).
	latestIndex := max(sessionCount-1, 0)

	for i := latestIndex; i >= 0; i-- {
		sessionPath := strconv.Itoa(i)
		sessionTree, err := cpTree.Tree(sessionPath)
		if err != nil {
			continue
		}

		file, err := sessionTree.File(paths.PromptFileName)
		if err != nil {
			continue
		}

		content, err := file.Contents()
		if err != nil {
			continue
		}

		if prompt := ExtractFirstPrompt(content); prompt != "" {
			return prompt
		}
	}

	return ""
}

// ReadAllSessionPromptsFromTree reads the first prompt for all sessions in a multi-session checkpoint.
// Returns a slice of prompts parallel to sessionIDs (oldest to newest).
// For single-session checkpoints, returns a slice with just the root prompt.
func ReadAllSessionPromptsFromTree(tree *object.Tree, checkpointPath string, sessionCount int, sessionIDs []string) []string {
	if sessionCount <= 1 || len(sessionIDs) <= 1 {
		// Single session - just return the root prompt
		prompt := ReadSessionPromptFromTree(tree, checkpointPath)
		if prompt != "" {
			return []string{prompt}
		}
		return nil
	}

	// Multi-session: read prompts from archived folders (0/, 1/, etc.) and root
	prompts := make([]string, len(sessionIDs))

	// Read archived session prompts (folders 0, 1, ... N-2)
	for i := range sessionCount - 1 {
		archivedPath := fmt.Sprintf("%s/%d", checkpointPath, i)
		prompts[i] = ReadSessionPromptFromTree(tree, archivedPath)
	}

	// Read the most recent session prompt (at root level)
	prompts[len(prompts)-1] = ReadSessionPromptFromTree(tree, checkpointPath)

	return prompts
}

// GetRemoteMetadataBranchTree returns the tree object for origin/entire/checkpoints/v1.
func GetRemoteMetadataBranchTree(repo *git.Repository) (*object.Tree, error) {
	refName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata branch reference: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata branch commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata branch tree: %w", err)
	}
	return tree, nil
}

// OpenRepository opens the git repository from the repo root.
// It uses 'git rev-parse --show-toplevel' to find the repository root,
// which works correctly even when called from a subdirectory or a linked worktree.
func OpenRepository(ctx context.Context) (*git.Repository, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to current directory if git command fails
		// (e.g., if git is not installed or we're not in a repo)
		repoRoot = "."
	}

	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
}

// IsInsideWorktree returns true if the current directory is inside a git worktree
// (as opposed to the main repository). Worktrees have .git as a file pointing
// to the main repo, while the main repo has .git as a directory.
// This function works correctly from any subdirectory within the repository.
func IsInsideWorktree(ctx context.Context) bool {
	// First find the repository root
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false
	}

	gitPath := filepath.Join(repoRoot, gitDir)
	gitInfo, err := os.Stat(gitPath)
	if err != nil {
		return false
	}
	return !gitInfo.IsDir()
}

// GetMainRepoRoot returns the root directory of the main repository.
// In the main repo, this is the worktree path (repo root).
// In a worktree, this parses the .git file to find the main repo.
// This function works correctly from any subdirectory within the repository.
//
// Per gitrepository-layout(5), a worktree's .git file is a "gitfile" containing
// "gitdir: <path>" pointing to $GIT_DIR/worktrees/<id> in the main repository.
// See: https://git-scm.com/docs/gitrepository-layout
func GetMainRepoRoot(ctx context.Context) (string, error) {
	// First find the worktree/repo root
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get worktree path: %w", err)
	}

	if !IsInsideWorktree(ctx) {
		return repoRoot, nil
	}

	// Worktree .git file contains: "gitdir: /path/to/main/.git/worktrees/<id>"
	gitFilePath := filepath.Join(repoRoot, gitDir)
	content, err := os.ReadFile(gitFilePath) //nolint:gosec // G304: gitFilePath is constructed from repo root, not user input
	if err != nil {
		return "", fmt.Errorf("failed to read .git file: %w", err)
	}

	gitdir := strings.TrimSpace(string(content))
	gitdir = strings.TrimPrefix(gitdir, "gitdir: ")

	// Extract main repo root: everything before "/.git/"
	idx := strings.LastIndex(gitdir, "/.git/")
	if idx < 0 {
		return "", fmt.Errorf("unexpected gitdir format: %s", gitdir)
	}
	return gitdir[:idx], nil
}

// GetGitCommonDir returns the path to the shared git directory.
// In a regular checkout, this is .git/
// In a worktree, this is the main repo's .git/ (not .git/worktrees/<name>/)
// Uses git rev-parse --git-common-dir for reliable handling of worktrees.
func GetGitCommonDir(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = "."
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}

	commonDir := strings.TrimSpace(string(output))

	// git rev-parse --git-common-dir returns relative paths from the working directory,
	// so we need to make it absolute if it isn't already
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(".", commonDir)
	}

	return filepath.Clean(commonDir), nil
}

// EnsureEntireGitignore ensures all required entries are in .entire/.gitignore
// Works correctly from any subdirectory within the repository.
func EnsureEntireGitignore(ctx context.Context) error {
	// Get absolute path for the gitignore file
	gitignoreAbs, err := paths.AbsPath(ctx, entireGitignore)
	if err != nil {
		gitignoreAbs = entireGitignore // Fallback to relative
	}

	// Read existing content
	var content string
	if data, err := os.ReadFile(gitignoreAbs); err == nil { //nolint:gosec // path is from AbsPath or constant
		content = string(data)
	}

	// All entries that should be in .entire/.gitignore
	requiredEntries := []string{
		"tmp/",
		"settings.local.json",
		"metadata/",
		"logs/",
	}

	// Track what needs to be added
	var toAdd []string
	for _, entry := range requiredEntries {
		if !strings.Contains(content, entry) {
			toAdd = append(toAdd, entry)
		}
	}

	// Nothing to add
	if len(toAdd) == 0 {
		return nil
	}

	// Ensure .entire directory exists
	if err := os.MkdirAll(filepath.Dir(gitignoreAbs), 0o750); err != nil {
		return fmt.Errorf("failed to create .entire directory: %w", err)
	}

	// Append missing entries to gitignore
	var sb strings.Builder
	for _, entry := range toAdd {
		sb.WriteString(entry + "\n")
	}
	content += sb.String()

	if err := os.WriteFile(gitignoreAbs, []byte(content), 0o644); err != nil { //nolint:gosec // path is from AbsPath or constant
		return fmt.Errorf("failed to write gitignore: %w", err)
	}
	return nil
}

// checkCanRewindWithWarning checks working directory and returns a warning with diff stats.
// Always returns canRewind=true but includes a warning message with +/- line stats for
// uncommitted changes. Used by manual-commit strategy.
func checkCanRewindWithWarning(ctx context.Context) (bool, string, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		// Can't open repo - still allow rewind but without stats
		return true, "", nil //nolint:nilerr // Rewind allowed even if repo can't be opened
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if worktree can't be accessed
	}

	status, err := worktree.Status()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if status can't be retrieved
	}

	if status.IsClean() {
		return true, "", nil
	}

	// Get HEAD commit tree for comparison - if we can't get it, just return without stats
	head, err := repo.Head()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even without HEAD (e.g., empty repo)
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if commit lookup fails
	}

	headTree, err := headCommit.Tree()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if tree lookup fails
	}

	type fileChange struct {
		status   string // "modified", "added", "deleted"
		added    int
		removed  int
		filename string
	}

	var changes []fileChange
	// Use repo root, not cwd - git status returns paths relative to repo root
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if worktree root lookup fails
	}

	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Skip untracked files
		if st.Worktree == git.Untracked {
			continue
		}

		var change fileChange
		change.filename = file

		switch {
		case st.Staging == git.Added || st.Worktree == git.Added:
			change.status = "added"
			// New file - count all lines as added
			absPath := filepath.Join(repoRoot, file)
			if content, err := os.ReadFile(absPath); err == nil { //nolint:gosec // absPath is repo root + relative path from git status
				change.added = countLines(content)
			}
		case st.Staging == git.Deleted || st.Worktree == git.Deleted:
			change.status = "deleted"
			// Deleted file - count lines from HEAD as removed
			if entry, err := headTree.File(file); err == nil {
				if content, err := entry.Contents(); err == nil {
					change.removed = countLines([]byte(content))
				}
			}
		case st.Staging == git.Modified || st.Worktree == git.Modified:
			change.status = "modified"
			// Modified file - compute diff stats
			var headContent, workContent []byte
			if entry, err := headTree.File(file); err == nil {
				if content, err := entry.Contents(); err == nil {
					headContent = []byte(content)
				}
			}
			absPath := filepath.Join(repoRoot, file)
			if content, err := os.ReadFile(absPath); err == nil { //nolint:gosec // absPath is repo root + relative path from git status
				workContent = content
			}
			if headContent != nil && workContent != nil {
				change.added, change.removed = computeDiffStats(headContent, workContent)
			}
		default:
			continue
		}

		changes = append(changes, change)
	}

	if len(changes) == 0 {
		return true, "", nil
	}

	// Sort changes by filename for consistent output
	sort.Slice(changes, func(i, j int) bool {
		return changes[i].filename < changes[j].filename
	})

	var msg strings.Builder
	msg.WriteString("The following uncommitted changes will be reverted:\n")

	totalAdded, totalRemoved := 0, 0
	for _, c := range changes {
		totalAdded += c.added
		totalRemoved += c.removed

		var stats string
		switch {
		case c.added > 0 && c.removed > 0:
			stats = fmt.Sprintf("+%d/-%d", c.added, c.removed)
		case c.added > 0:
			stats = fmt.Sprintf("+%d", c.added)
		case c.removed > 0:
			stats = fmt.Sprintf("-%d", c.removed)
		}

		fmt.Fprintf(&msg, "  %-10s %s", c.status+":", c.filename)
		if stats != "" {
			fmt.Fprintf(&msg, " (%s)", stats)
		}
		msg.WriteString("\n")
	}

	if totalAdded > 0 || totalRemoved > 0 {
		fmt.Fprintf(&msg, "\nTotal: +%d/-%d lines\n", totalAdded, totalRemoved)
	}

	return true, msg.String(), nil
}

// countLines counts the number of lines in content.
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	count := 1
	for _, b := range content {
		if b == '\n' {
			count++
		}
	}
	// Don't count trailing newline as extra line
	if len(content) > 0 && content[len(content)-1] == '\n' {
		count--
	}
	return count
}

// computeDiffStats computes added and removed line counts between old and new content.
// Uses a simple line-based diff algorithm.
func computeDiffStats(oldContent, newContent []byte) (added, removed int) {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)

	// Build a set of old lines with counts
	oldSet := make(map[string]int)
	for _, line := range oldLines {
		oldSet[line]++
	}

	// Check which new lines are truly new
	for _, line := range newLines {
		if oldSet[line] > 0 {
			oldSet[line]--
		} else {
			added++
		}
	}

	// Remaining old lines are removed
	for _, count := range oldSet {
		removed += count
	}

	return added, removed
}

// splitLines splits content into lines, preserving empty lines.
// Handles both Unix (\n) and Windows (\r\n) line endings.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	s := string(content)
	// Normalize Windows line endings to Unix
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Remove trailing newline to avoid empty last element
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// fileExists checks if a file exists at the given path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// getTaskCheckpointFromTree retrieves a task checkpoint from a commit tree.
// Shared implementation for shadow and linear-shadow strategies.
func getTaskCheckpointFromTree(ctx context.Context, point RewindPoint) (*TaskCheckpoint, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// Read checkpoint.json from the tree
	checkpointPath := point.MetadataDir + "/checkpoint.json"
	file, err := tree.File(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find checkpoint at %s: %w", checkpointPath, err)
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var checkpoint TaskCheckpoint
	if err := json.Unmarshal([]byte(content), &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint: %w", err)
	}

	return &checkpoint, nil
}

// getTaskTranscriptFromTree retrieves a task transcript from a commit tree.
// Shared implementation for shadow and linear-shadow strategies.
func getTaskTranscriptFromTree(ctx context.Context, point RewindPoint) ([]byte, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// MetadataDir format: .entire/metadata/<session>/tasks/<toolUseID>
	// Session transcript is at: .entire/metadata/<session>/<TranscriptFileName>
	sessionDir := filepath.Dir(filepath.Dir(point.MetadataDir))

	// Try current format first, then legacy
	transcriptPath := sessionDir + "/" + paths.TranscriptFileName
	file, err := tree.File(transcriptPath)
	if err != nil {
		transcriptPath = sessionDir + "/" + paths.TranscriptFileNameLegacy
		file, err = tree.File(transcriptPath)
		if err != nil {
			return nil, fmt.Errorf("failed to find transcript: %w", err)
		}
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	return []byte(content), nil
}

// ErrBranchNotFound is returned by DeleteBranchCLI when the branch does not exist.
var ErrBranchNotFound = errors.New("branch not found")

// DeleteBranchCLI deletes a git branch using the git CLI.
// Uses `git branch -D` instead of go-git's RemoveReference because go-git v5
// doesn't properly persist deletions when refs are packed (.git/packed-refs)
// or in a worktree context. This is the same class of go-git v5 bug that
// affects checkout and reset --hard (see HardResetWithProtection).
//
// Returns ErrBranchNotFound if the branch does not exist, allowing callers
// to use errors.Is for idempotent deletion patterns.
func DeleteBranchCLI(ctx context.Context, branchName string) error {
	// Pre-check: verify the branch exists so callers get a structured error
	// instead of parsing git's output string (which varies across locales).
	// git show-ref exits 1 for "not found" and 128+ for fatal errors (corrupt
	// repo, permissions, not a git directory). Only map exit code 1 to
	// ErrBranchNotFound; propagate other failures as-is.
	check := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	if err := check.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return fmt.Errorf("%w: %s", ErrBranchNotFound, branchName)
		}
		return fmt.Errorf("failed to check branch %s: %w", branchName, err)
	}

	cmd := exec.CommandContext(ctx, "git", "branch", "-D", "--", branchName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete branch %s: %s: %w", branchName, strings.TrimSpace(string(output)), err)
	}
	return nil
}

// branchExistsCLI checks if a branch exists using git CLI.
// Returns nil if the branch exists, or an error if it does not.
func branchExistsCLI(ctx context.Context, branchName string) error {
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("branch %s not found: %w", branchName, err)
	}
	return nil
}

// HardResetWithProtection performs a git reset --hard to the specified commit.
// Uses the git CLI instead of go-git because go-git's HardReset incorrectly
// deletes untracked directories (like .entire/) even when they're in .gitignore.
// Returns the short commit ID (7 chars) on success for display purposes.
func HardResetWithProtection(ctx context.Context, commitHash plumbing.Hash) (shortID string, err error) {
	hashStr := commitHash.String()
	cmd := exec.CommandContext(ctx, "git", "reset", "--hard", hashStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("reset failed: %s: %w", strings.TrimSpace(string(output)), err)
	}

	// Return short commit ID for display
	shortID = hashStr
	if len(shortID) > 7 {
		shortID = shortID[:7]
	}
	return shortID, nil
}

// collectUntrackedFiles collects untracked files in the working directory that are
// NOT ignored by .gitignore. This is used to capture the initial state when starting
// a session, ensuring untracked files present at session start are preserved during rewind.
// Uses "git ls-files --others --exclude-standard -z" to respect .gitignore rules,
// avoiding bloated session state from large ignored directories like node_modules/.
// Returns paths relative to the repository root.
func collectUntrackedFiles(ctx context.Context) ([]string, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	cmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard", "-z")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git ls-files failed: %s: %w", strings.TrimSpace(string(exitErr.Stderr)), err)
		}
		return nil, fmt.Errorf("git ls-files failed: %w", err)
	}

	raw := string(output)
	if raw == "" {
		return nil, nil
	}

	var files []string
	for _, f := range strings.Split(raw, "\x00") {
		// Defense-in-depth: filter protected paths even though --exclude-standard should already handle them
		if f != "" && !isProtectedPath(f) {
			files = append(files, f)
		}
	}
	return files, nil
}

// ExtractSessionIDFromCommit extracts the session ID from a commit's trailers.
// It checks the Entire-Session trailer first, then falls back to extracting from
// the metadata directory path in the Entire-Metadata trailer.
// Returns empty string if no session ID is found.
func ExtractSessionIDFromCommit(commit *object.Commit) string {
	// Try Entire-Session trailer first
	if sessionID, found := trailers.ParseSession(commit.Message); found {
		return sessionID
	}

	// Try extracting from metadata directory (last path component)
	if metadataDir, found := trailers.ParseMetadata(commit.Message); found {
		return filepath.Base(metadataDir)
	}

	return ""
}

// NOTE: The following git tree helper functions have been moved to checkpoint/ package:
// - FlattenTree -> checkpoint.FlattenTree
// - CreateBlobFromContent -> checkpoint.CreateBlobFromContent
// - BuildTreeFromEntries -> checkpoint.BuildTreeFromEntries
// - sortTreeEntries (internal to checkpoint package)
// - treeNode, insertIntoTree, buildTreeObject (internal to checkpoint package)
//
// See push_common.go and session_test.go for usage examples.

// createCommit creates a commit object
func createCommit(repo *git.Repository, treeHash, parentHash plumbing.Hash, message, authorName, authorEmail string) (plumbing.Hash, error) { //nolint:unparam // already present in codebase
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

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store commit: %w", err)
	}

	return hash, nil
}

// getSessionDescriptionFromTree reads the first line of prompt.txt from a git tree.
// This is the tree-based equivalent of getSessionDescription (which reads from filesystem).
//
// If metadataDir is provided, looks for files at metadataDir/prompt.txt.
// If metadataDir is empty, first tries the root of the tree (for when the tree is already
// the session directory), then falls back to
// searching for .entire/metadata/*/prompt.txt (for full worktree trees).
func getSessionDescriptionFromTree(tree *object.Tree, metadataDir string) string {
	// Helper to read first line from a file in tree
	readFirstLine := func(path string) string {
		file, err := tree.File(path)
		if err != nil {
			return ""
		}
		content, err := file.Contents()
		if err != nil {
			return ""
		}
		lines := strings.SplitN(content, "\n", 2)
		if len(lines) > 0 && lines[0] != "" {
			return strings.TrimSpace(lines[0])
		}
		return ""
	}

	// If metadataDir is provided, look there directly
	if metadataDir != "" {
		if desc := readFirstLine(metadataDir + "/" + paths.PromptFileName); desc != "" {
			return desc
		}
		return NoDescription
	}

	// No metadataDir provided - first try looking at the root of the tree
	// (used when the tree is already the session directory)
	if desc := readFirstLine(paths.PromptFileName); desc != "" {
		return desc
	}

	// Fall back to searching for .entire/metadata/*/prompt.txt
	// (used when the tree is the full worktree)
	var desc string
	//nolint:errcheck // We ignore errors here as we're just searching for a description
	_ = tree.Files().ForEach(func(f *object.File) error {
		if desc != "" {
			return nil // Already found description
		}
		name := f.Name
		if strings.Contains(name, ".entire/metadata/") && strings.HasSuffix(name, "/"+paths.PromptFileName) {
			content, err := f.Contents()
			if err != nil {
				return nil //nolint:nilerr // Skip files we can't read, continue searching
			}
			lines := strings.SplitN(content, "\n", 2)
			if len(lines) > 0 && lines[0] != "" {
				desc = strings.TrimSpace(lines[0])
			}
		}
		return nil
	})

	if desc != "" {
		return desc
	}
	return NoDescription
}

// GetGitAuthorFromRepo retrieves the git user.name and user.email,
// checking both the repository-local config and the global ~/.gitconfig.
// Delegates to checkpoint.GetGitAuthorFromRepo — this wrapper exists so
// callers within the strategy package don't need a qualified import.
func GetGitAuthorFromRepo(repo *git.Repository) (name, email string) {
	return checkpoint.GetGitAuthorFromRepo(repo)
}

// GetCurrentBranchName returns the short name of the current branch if HEAD points to a branch.
// Returns an empty string if in detached HEAD state or if there's an error reading HEAD.
// This is used to capture branch metadata for checkpoints.
func GetCurrentBranchName(repo *git.Repository) string {
	head, err := repo.Head()
	if err != nil || !head.Name().IsBranch() {
		return ""
	}
	return head.Name().Short()
}

// getMainBranchHash returns the hash of the main branch (main or master).
// Returns ZeroHash if no main branch is found.
func GetMainBranchHash(repo *git.Repository) plumbing.Hash {
	// Try common main branch names
	for _, branchName := range []string{branchMain, branchMaster} {
		// Try local branch first
		ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
		if err == nil {
			return ref.Hash()
		}
		// Try remote tracking branch
		ref, err = repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
		if err == nil {
			return ref.Hash()
		}
	}
	return plumbing.ZeroHash
}

// GetDefaultBranchName returns the name of the default branch.
// First checks origin/HEAD, then falls back to checking if main/master exists.
// Returns empty string if unable to determine.
// NOTE: Duplicated from cli/git_operations.go - see ENT-129 for consolidation.
func GetDefaultBranchName(repo *git.Repository) string {
	// Try to get the symbolic reference for origin/HEAD
	// Use resolved=false to get the symbolic ref itself, then extract its target
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), false)
	if err == nil && ref != nil && ref.Type() == plumbing.SymbolicReference {
		target := ref.Target().String()
		if branchName, found := strings.CutPrefix(target, "refs/remotes/origin/"); found {
			return branchName
		}
	}

	// Fallback: check if origin/main or origin/master exists
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchMain), true); err == nil {
		return branchMain
	}
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchMaster), true); err == nil {
		return branchMaster
	}

	// Final fallback: check local branches
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(branchMain), true); err == nil {
		return branchMain
	}
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(branchMaster), true); err == nil {
		return branchMaster
	}

	return ""
}

// IsOnDefaultBranch checks if the repository HEAD is on the default branch.
// Returns (isOnDefault, currentBranchName).
// NOTE: Duplicated from cli/git_operations.go - see ENT-129 for consolidation.
func IsOnDefaultBranch(repo *git.Repository) (bool, string) {
	currentBranch := GetCurrentBranchName(repo)
	if currentBranch == "" {
		return false, ""
	}

	defaultBranch := GetDefaultBranchName(repo)
	if defaultBranch == "" {
		// Can't determine default, check common names
		if currentBranch == branchMain || currentBranch == branchMaster {
			return true, currentBranch
		}
		return false, currentBranch
	}

	return currentBranch == defaultBranch, currentBranch
}

// prepareTranscriptForState ensures the transcript is up-to-date for the given session.
// Only prepares for ACTIVE sessions — IDLE/ENDED sessions are already flushed.
// Resolves the agent from state.AgentType internally. Multiple calls are safe but
// not free — callers should avoid redundant calls for performance.
func prepareTranscriptForState(ctx context.Context, state *SessionState) {
	if !state.Phase.IsActive() || state.TranscriptPath == "" || state.AgentType == "" {
		return
	}
	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		logging.Debug(ctx, "prepareTranscriptForState: unknown agent type",
			slog.String("session_id", state.SessionID),
			slog.String("agent_type", string(state.AgentType)),
			slog.Any("error", err),
		)
		return
	}
	prepareTranscriptIfNeeded(ctx, ag, state.TranscriptPath)
}

// prepareTranscriptIfNeeded calls PrepareTranscript for agents that implement
// the TranscriptPreparer interface. This ensures transcript files exist before
// they are read (e.g., OpenCode creates its transcript lazily via `opencode export`).
// Errors are silently ignored — this is best-effort for hook paths.
func prepareTranscriptIfNeeded(ctx context.Context, ag agent.Agent, transcriptPath string) {
	if ag == nil || transcriptPath == "" {
		return
	}
	if preparer, ok := agent.AsTranscriptPreparer(ag); ok {
		// Best-effort: callers handle missing files gracefully.
		// Transcript may not be available yet (e.g., agent not installed).
		_ = preparer.PrepareTranscript(ctx, transcriptPath) //nolint:errcheck // Best-effort in hook path
	}
}
