package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/binary"
)

// errStopIteration is used to stop commit iteration early in GetCheckpointAuthor.
var errStopIteration = errors.New("stop iteration")

// WriteCommitted writes a committed checkpoint to the entire/checkpoints/v1 branch.
// Checkpoints are stored at sharded paths: <id[:2]>/<id[2:]>/
//
// For task checkpoints (IsTask=true), additional files are written under tasks/<tool-use-id>/:
//   - For incremental checkpoints: checkpoints/NNN-<tool-use-id>.json
//   - For final checkpoints: checkpoint.json and agent-<agent-id>.jsonl
func (s *GitStore) WriteCommitted(ctx context.Context, opts WriteCommittedOptions) error {
	_ = ctx // Reserved for future use

	// Validate identifiers to prevent path traversal and malformed data
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid checkpoint options: checkpoint ID is required")
	}
	if err := validation.ValidateSessionID(opts.SessionID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}
	if err := validation.ValidateToolUseID(opts.ToolUseID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}
	if err := validation.ValidateAgentID(opts.AgentID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}

	// Ensure sessions branch exists
	if err := s.ensureSessionsBranch(); err != nil {
		return fmt.Errorf("failed to ensure sessions branch: %w", err)
	}

	// Get current branch tip and flatten tree
	ref, entries, err := s.getSessionsBranchEntries()
	if err != nil {
		return err
	}

	// Use sharded path: <id[:2]>/<id[2:]>/
	basePath := opts.CheckpointID.Path() + "/"

	// Track task metadata path for commit trailer
	var taskMetadataPath string

	// Handle task checkpoints
	if opts.IsTask && opts.ToolUseID != "" {
		taskMetadataPath, err = s.writeTaskCheckpointEntries(opts, basePath, entries)
		if err != nil {
			return err
		}
	}

	// Write standard checkpoint entries (transcript, prompts, context, metadata)
	if err := s.writeStandardCheckpointEntries(opts, basePath, entries); err != nil {
		return err
	}

	// Build and commit
	newTreeHash, err := BuildTreeFromEntries(s.repo, entries)
	if err != nil {
		return err
	}

	commitMsg := s.buildCommitMessage(opts, taskMetadataPath)
	newCommitHash, err := s.createCommit(newTreeHash, ref.Hash(), commitMsg, opts.AuthorName, opts.AuthorEmail)
	if err != nil {
		return err
	}

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	newRef := plumbing.NewHashReference(refName, newCommitHash)
	if err := s.repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to set branch reference: %w", err)
	}

	return nil
}

// getSessionsBranchEntries returns the sessions branch reference and flattened tree entries.
func (s *GitStore) getSessionsBranchEntries() (*plumbing.Reference, map[string]object.TreeEntry, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get sessions branch reference: %w", err)
	}

	parentCommit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	baseTree, err := parentCommit.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	entries := make(map[string]object.TreeEntry)
	if err := FlattenTree(s.repo, baseTree, "", entries); err != nil {
		return nil, nil, err
	}

	return ref, entries, nil
}

// writeTaskCheckpointEntries writes task-specific checkpoint entries and returns the task metadata path.
func (s *GitStore) writeTaskCheckpointEntries(opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry) (string, error) {
	taskPath := basePath + "tasks/" + opts.ToolUseID + "/"

	if opts.IsIncremental {
		return s.writeIncrementalTaskCheckpoint(opts, taskPath, entries)
	}
	return s.writeFinalTaskCheckpoint(opts, taskPath, entries)
}

// writeIncrementalTaskCheckpoint writes an incremental checkpoint file during task execution.
func (s *GitStore) writeIncrementalTaskCheckpoint(opts WriteCommittedOptions, taskPath string, entries map[string]object.TreeEntry) (string, error) {
	incData, err := redact.JSONLBytes(opts.IncrementalData)
	if err != nil {
		return "", fmt.Errorf("failed to redact incremental checkpoint: %w", err)
	}
	checkpoint := incrementalCheckpointData{
		Type:      opts.IncrementalType,
		ToolUseID: opts.ToolUseID,
		Timestamp: time.Now().UTC(),
		Data:      incData,
	}
	cpData, err := jsonutil.MarshalIndentWithNewline(checkpoint, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal incremental checkpoint: %w", err)
	}
	cpBlobHash, err := CreateBlobFromContent(s.repo, cpData)
	if err != nil {
		return "", fmt.Errorf("failed to create incremental checkpoint blob: %w", err)
	}

	cpFilename := fmt.Sprintf("%03d-%s.json", opts.IncrementalSequence, opts.ToolUseID)
	cpPath := taskPath + "checkpoints/" + cpFilename
	entries[cpPath] = object.TreeEntry{
		Name: cpPath,
		Mode: filemode.Regular,
		Hash: cpBlobHash,
	}
	return cpPath, nil
}

// writeFinalTaskCheckpoint writes the final checkpoint.json and subagent transcript.
func (s *GitStore) writeFinalTaskCheckpoint(opts WriteCommittedOptions, taskPath string, entries map[string]object.TreeEntry) (string, error) {
	checkpoint := taskCheckpointData{
		SessionID:      opts.SessionID,
		ToolUseID:      opts.ToolUseID,
		CheckpointUUID: opts.CheckpointUUID,
		AgentID:        opts.AgentID,
	}
	checkpointData, err := jsonutil.MarshalIndentWithNewline(checkpoint, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal task checkpoint: %w", err)
	}
	blobHash, err := CreateBlobFromContent(s.repo, checkpointData)
	if err != nil {
		return "", fmt.Errorf("failed to create task checkpoint blob: %w", err)
	}

	checkpointFile := taskPath + "checkpoint.json"
	entries[checkpointFile] = object.TreeEntry{
		Name: checkpointFile,
		Mode: filemode.Regular,
		Hash: blobHash,
	}

	// Write subagent transcript if available
	if opts.SubagentTranscriptPath != "" && opts.AgentID != "" {
		agentContent, readErr := os.ReadFile(opts.SubagentTranscriptPath)
		if readErr == nil {
			agentContent, readErr = redact.JSONLBytes(agentContent)
		}
		if readErr == nil {
			agentBlobHash, agentBlobErr := CreateBlobFromContent(s.repo, agentContent)
			if agentBlobErr == nil {
				agentPath := taskPath + "agent-" + opts.AgentID + ".jsonl"
				entries[agentPath] = object.TreeEntry{
					Name: agentPath,
					Mode: filemode.Regular,
					Hash: agentBlobHash,
				}
			}
		}
	}

	// Return task path without trailing slash
	return taskPath[:len(taskPath)-1], nil
}

// writeStandardCheckpointEntries writes session files to numbered subdirectories and
// maintains a CheckpointSummary at the root level with aggregated statistics.
//
// Structure:
//
//	basePath/
//	├── metadata.json         # CheckpointSummary (aggregated stats)
//	├── 1/                    # First session
//	│   ├── metadata.json     # CommittedMetadata (session-specific, includes initial_attribution)
//	│   ├── full.jsonl
//	│   ├── prompt.txt
//	│   ├── context.md
//	│   └── content_hash.txt
//	├── 2/                    # Second session
//	└── ...
func (s *GitStore) writeStandardCheckpointEntries(opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry) error {
	// Read existing summary to get current session count
	var existingSummary *CheckpointSummary
	metadataPath := basePath + paths.MetadataFileName
	if entry, exists := entries[metadataPath]; exists {
		existing, err := s.readSummaryFromBlob(entry.Hash)
		if err == nil {
			existingSummary = existing
		}
	}

	// Determine session index: reuse existing slot if session ID matches, otherwise append
	sessionIndex := s.findSessionIndex(basePath, existingSummary, entries, opts.SessionID)

	// Write session files to numbered subdirectory
	sessionPath := fmt.Sprintf("%s%d/", basePath, sessionIndex)
	sessionFilePaths, err := s.writeSessionToSubdirectory(opts, sessionPath, entries)
	if err != nil {
		return err
	}

	// Copy additional metadata files from directory if specified (to session subdirectory)
	if opts.MetadataDir != "" {
		if err := s.copyMetadataDir(opts.MetadataDir, sessionPath, entries); err != nil {
			return fmt.Errorf("failed to copy metadata directory: %w", err)
		}
	}

	// Build the sessions array
	var sessions []SessionFilePaths
	if existingSummary != nil {
		sessions = make([]SessionFilePaths, max(len(existingSummary.Sessions), sessionIndex+1))
		copy(sessions, existingSummary.Sessions)
	} else {
		sessions = make([]SessionFilePaths, 1)
	}
	sessions[sessionIndex] = sessionFilePaths

	// Update root metadata.json with CheckpointSummary
	return s.writeCheckpointSummary(opts, basePath, entries, sessions)
}

// writeSessionToSubdirectory writes a single session's files to a numbered subdirectory.
// Returns the absolute file paths from the git tree root for the sessions map.
func (s *GitStore) writeSessionToSubdirectory(opts WriteCommittedOptions, sessionPath string, entries map[string]object.TreeEntry) (SessionFilePaths, error) {
	filePaths := SessionFilePaths{}

	// Clear any existing entries at this path so stale files from a previous
	// write (e.g. prompt.txt, context.md) don't persist on overwrite.
	for key := range entries {
		if strings.HasPrefix(key, sessionPath) {
			delete(entries, key)
		}
	}

	// Write transcript
	if err := s.writeTranscript(opts, sessionPath, entries); err != nil {
		return filePaths, err
	}
	filePaths.Transcript = "/" + sessionPath + paths.TranscriptFileName
	filePaths.ContentHash = "/" + sessionPath + paths.ContentHashFileName

	// Write prompts
	if len(opts.Prompts) > 0 {
		promptContent := redact.String(strings.Join(opts.Prompts, "\n\n---\n\n"))
		blobHash, err := CreateBlobFromContent(s.repo, []byte(promptContent))
		if err != nil {
			return filePaths, err
		}
		entries[sessionPath+paths.PromptFileName] = object.TreeEntry{
			Name: sessionPath + paths.PromptFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
		filePaths.Prompt = "/" + sessionPath + paths.PromptFileName
	}

	// Write context
	if len(opts.Context) > 0 {
		blobHash, err := CreateBlobFromContent(s.repo, redact.Bytes(opts.Context))
		if err != nil {
			return filePaths, err
		}
		entries[sessionPath+paths.ContextFileName] = object.TreeEntry{
			Name: sessionPath + paths.ContextFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
		filePaths.Context = "/" + sessionPath + paths.ContextFileName
	}

	// Write session-level metadata.json (CommittedMetadata with all fields including initial_attribution)
	sessionMetadata := CommittedMetadata{
		CheckpointID:                opts.CheckpointID,
		SessionID:                   opts.SessionID,
		Strategy:                    opts.Strategy,
		CreatedAt:                   time.Now().UTC(),
		Branch:                      opts.Branch,
		CheckpointsCount:            opts.CheckpointsCount,
		FilesTouched:                opts.FilesTouched,
		Agent:                       opts.Agent,
		IsTask:                      opts.IsTask,
		ToolUseID:                   opts.ToolUseID,
		TranscriptIdentifierAtStart: opts.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   opts.CheckpointTranscriptStart,
		TranscriptLinesAtStart:      opts.CheckpointTranscriptStart, // Deprecated: kept for backward compat
		TokenUsage:                  opts.TokenUsage,
		InitialAttribution:          opts.InitialAttribution,
		Summary:                     opts.Summary,
	}

	metadataJSON, err := jsonutil.MarshalIndentWithNewline(sessionMetadata, "", "  ")
	if err != nil {
		return filePaths, fmt.Errorf("failed to marshal session metadata: %w", err)
	}
	metadataHash, err := CreateBlobFromContent(s.repo, metadataJSON)
	if err != nil {
		return filePaths, err
	}
	entries[sessionPath+paths.MetadataFileName] = object.TreeEntry{
		Name: sessionPath + paths.MetadataFileName,
		Mode: filemode.Regular,
		Hash: metadataHash,
	}
	filePaths.Metadata = "/" + sessionPath + paths.MetadataFileName

	return filePaths, nil
}

// writeCheckpointSummary writes the root-level CheckpointSummary with aggregated statistics.
// sessions is the complete sessions array (already built by the caller).
func (s *GitStore) writeCheckpointSummary(opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry, sessions []SessionFilePaths) error {
	checkpointsCount, filesTouched, tokenUsage, err :=
		s.reaggregateFromEntries(basePath, len(sessions), entries)
	if err != nil {
		return fmt.Errorf("failed to aggregate session stats: %w", err)
	}

	summary := CheckpointSummary{
		CheckpointID:     opts.CheckpointID,
		Strategy:         opts.Strategy,
		Branch:           opts.Branch,
		CheckpointsCount: checkpointsCount,
		FilesTouched:     filesTouched,
		Sessions:         sessions,
		TokenUsage:       tokenUsage,
	}

	metadataJSON, err := jsonutil.MarshalIndentWithNewline(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint summary: %w", err)
	}
	metadataHash, err := CreateBlobFromContent(s.repo, metadataJSON)
	if err != nil {
		return err
	}
	entries[basePath+paths.MetadataFileName] = object.TreeEntry{
		Name: basePath + paths.MetadataFileName,
		Mode: filemode.Regular,
		Hash: metadataHash,
	}
	return nil
}

// findSessionIndex returns the index of an existing session with the given ID,
// or the next available index if not found. This prevents duplicate session entries.
func (s *GitStore) findSessionIndex(basePath string, existingSummary *CheckpointSummary, entries map[string]object.TreeEntry, sessionID string) int {
	if existingSummary == nil {
		return 0
	}
	for i := range len(existingSummary.Sessions) {
		path := fmt.Sprintf("%s%d/%s", basePath, i, paths.MetadataFileName)
		if entry, exists := entries[path]; exists {
			meta, err := s.readMetadataFromBlob(entry.Hash)
			if err != nil {
				logging.Warn(context.Background(), "failed to read session metadata during dedup check",
					slog.Int("session_index", i),
					slog.String("session_id", sessionID),
					slog.String("error", err.Error()),
				)
				continue
			}
			if meta.SessionID == sessionID {
				return i
			}
		}
	}
	return len(existingSummary.Sessions)
}

// reaggregateFromEntries reads all session metadata from the entries map and
// reaggregates CheckpointsCount, FilesTouched, and TokenUsage.
func (s *GitStore) reaggregateFromEntries(basePath string, sessionCount int, entries map[string]object.TreeEntry) (int, []string, *agent.TokenUsage, error) {
	var totalCount int
	var allFiles []string
	var totalTokens *agent.TokenUsage

	for i := range sessionCount {
		path := fmt.Sprintf("%s%d/%s", basePath, i, paths.MetadataFileName)
		entry, exists := entries[path]
		if !exists {
			return 0, nil, nil, fmt.Errorf("session %d metadata not found at %s", i, path)
		}
		meta, err := s.readMetadataFromBlob(entry.Hash)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("failed to read session %d metadata: %w", i, err)
		}
		totalCount += meta.CheckpointsCount
		allFiles = mergeFilesTouched(allFiles, meta.FilesTouched)
		totalTokens = aggregateTokenUsage(totalTokens, meta.TokenUsage)
	}

	return totalCount, allFiles, totalTokens, nil
}

// readJSONFromBlob reads JSON from a blob hash and decodes it to the given type.
func readJSONFromBlob[T any](repo *git.Repository, hash plumbing.Hash) (*T, error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get blob: %w", err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to get blob reader: %w", err)
	}
	defer reader.Close()

	var result T
	if err := json.NewDecoder(reader).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode: %w", err)
	}

	return &result, nil
}

// readSummaryFromBlob reads CheckpointSummary from a blob hash.
func (s *GitStore) readSummaryFromBlob(hash plumbing.Hash) (*CheckpointSummary, error) {
	return readJSONFromBlob[CheckpointSummary](s.repo, hash)
}

// aggregateTokenUsage sums two TokenUsage structs.
// Returns nil if both inputs are nil.
func aggregateTokenUsage(a, b *agent.TokenUsage) *agent.TokenUsage {
	if a == nil && b == nil {
		return nil
	}
	result := &agent.TokenUsage{}
	if a != nil {
		result.InputTokens = a.InputTokens
		result.CacheCreationTokens = a.CacheCreationTokens
		result.CacheReadTokens = a.CacheReadTokens
		result.OutputTokens = a.OutputTokens
		result.APICallCount = a.APICallCount
	}
	if b != nil {
		result.InputTokens += b.InputTokens
		result.CacheCreationTokens += b.CacheCreationTokens
		result.CacheReadTokens += b.CacheReadTokens
		result.OutputTokens += b.OutputTokens
		result.APICallCount += b.APICallCount
	}
	return result
}

// writeTranscript writes the transcript file from in-memory content or file path.
// If the transcript exceeds MaxChunkSize, it's split into multiple chunk files.
func (s *GitStore) writeTranscript(opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry) error {
	transcript := opts.Transcript
	if len(transcript) == 0 && opts.TranscriptPath != "" {
		var readErr error
		transcript, readErr = os.ReadFile(opts.TranscriptPath)
		if readErr != nil {
			// Non-fatal: transcript may not exist yet
			transcript = nil
		}
	}
	if len(transcript) == 0 {
		return nil
	}

	// Redact secrets before chunking so content hash reflects redacted content
	transcript, err := redact.JSONLBytes(transcript)
	if err != nil {
		return fmt.Errorf("failed to redact transcript secrets: %w", err)
	}

	// Chunk the transcript if it's too large
	chunks, err := agent.ChunkTranscript(transcript, opts.Agent)
	if err != nil {
		return fmt.Errorf("failed to chunk transcript: %w", err)
	}

	// Write chunk files
	for i, chunk := range chunks {
		chunkPath := basePath + agent.ChunkFileName(paths.TranscriptFileName, i)
		blobHash, err := CreateBlobFromContent(s.repo, chunk)
		if err != nil {
			return err
		}
		entries[chunkPath] = object.TreeEntry{
			Name: chunkPath,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	// Content hash for deduplication (hash of full transcript)
	contentHash := fmt.Sprintf("sha256:%x", sha256.Sum256(transcript))
	hashBlob, err := CreateBlobFromContent(s.repo, []byte(contentHash))
	if err != nil {
		return err
	}
	entries[basePath+paths.ContentHashFileName] = object.TreeEntry{
		Name: basePath + paths.ContentHashFileName,
		Mode: filemode.Regular,
		Hash: hashBlob,
	}
	return nil
}

// mergeFilesTouched combines two file lists, removing duplicates.
func mergeFilesTouched(existing, additional []string) []string {
	seen := make(map[string]bool)
	var result []string

	for _, f := range existing {
		if !seen[f] {
			seen[f] = true
			result = append(result, f)
		}
	}
	for _, f := range additional {
		if !seen[f] {
			seen[f] = true
			result = append(result, f)
		}
	}

	sort.Strings(result)
	return result
}

// readMetadataFromBlob reads CommittedMetadata from a blob hash.
func (s *GitStore) readMetadataFromBlob(hash plumbing.Hash) (*CommittedMetadata, error) {
	return readJSONFromBlob[CommittedMetadata](s.repo, hash)
}

// buildCommitMessage constructs the commit message with proper trailers.
// The commit subject is always "Checkpoint: <id>" for consistency.
// If CommitSubject is provided (e.g., for task checkpoints), it's included in the body.
func (s *GitStore) buildCommitMessage(opts WriteCommittedOptions, taskMetadataPath string) string {
	var commitMsg strings.Builder

	// Subject line is always the checkpoint ID for consistent formatting
	commitMsg.WriteString(fmt.Sprintf("Checkpoint: %s\n\n", opts.CheckpointID))

	// Include custom description in body if provided (e.g., task checkpoint details)
	if opts.CommitSubject != "" {
		commitMsg.WriteString(opts.CommitSubject + "\n\n")
	}
	commitMsg.WriteString(fmt.Sprintf("%s: %s\n", trailers.SessionTrailerKey, opts.SessionID))
	commitMsg.WriteString(fmt.Sprintf("%s: %s\n", trailers.StrategyTrailerKey, opts.Strategy))
	if opts.Agent != "" {
		commitMsg.WriteString(fmt.Sprintf("%s: %s\n", trailers.AgentTrailerKey, opts.Agent))
	}
	if opts.EphemeralBranch != "" {
		commitMsg.WriteString(fmt.Sprintf("%s: %s\n", trailers.EphemeralBranchTrailerKey, opts.EphemeralBranch))
	}
	if taskMetadataPath != "" {
		commitMsg.WriteString(fmt.Sprintf("%s: %s\n", trailers.MetadataTaskTrailerKey, taskMetadataPath))
	}

	return commitMsg.String()
}

// incrementalCheckpointData represents an incremental checkpoint during subagent execution.
// This mirrors strategy.SubagentCheckpoint but avoids import cycles.
type incrementalCheckpointData struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// taskCheckpointData represents a final task checkpoint.
// This mirrors strategy.TaskCheckpoint but avoids import cycles.
type taskCheckpointData struct {
	SessionID      string `json:"session_id"`
	ToolUseID      string `json:"tool_use_id"`
	CheckpointUUID string `json:"checkpoint_uuid"`
	AgentID        string `json:"agent_id,omitempty"`
}

// ReadCommitted reads a committed checkpoint's summary by ID from the entire/checkpoints/v1 branch.
// Returns only the CheckpointSummary (paths + aggregated stats), not actual content.
// Use ReadSessionContent to read actual transcript/prompts/context.
// Returns nil, nil if the checkpoint doesn't exist.
//
// The storage format uses numbered subdirectories for each session (0-based):
//
//	<checkpoint-id>/
//	├── metadata.json      # CheckpointSummary with sessions map
//	├── 0/                 # First session
//	│   ├── metadata.json  # Session-specific metadata
//	│   └── full.jsonl     # Transcript
//	├── 1/                 # Second session
//	└── ...
func (s *GitStore) ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	_ = ctx // Reserved for future use

	tree, err := s.getSessionsBranchTree()
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // No sessions branch means no checkpoint exists
	}

	checkpointPath := checkpointID.Path()
	checkpointTree, err := tree.Tree(checkpointPath)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Checkpoint directory not found
	}

	// Read root metadata.json as CheckpointSummary
	metadataFile, err := checkpointTree.File(paths.MetadataFileName)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // metadata.json not found
	}

	content, err := metadataFile.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata.json: %w", err)
	}

	var summary CheckpointSummary
	if err := json.Unmarshal([]byte(content), &summary); err != nil {
		return nil, fmt.Errorf("failed to parse metadata.json: %w", err)
	}

	return &summary, nil
}

// ReadSessionContent reads the actual content for a specific session within a checkpoint.
// sessionIndex is 0-based (0 for first session, 1 for second, etc.).
// Returns the session's metadata, transcript, prompts, and context.
// Returns an error if the checkpoint or session doesn't exist.
func (s *GitStore) ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	_ = ctx // Reserved for future use

	tree, err := s.getSessionsBranchTree()
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	checkpointPath := checkpointID.Path()
	checkpointTree, err := tree.Tree(checkpointPath)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	// Get the session subdirectory
	sessionDir := strconv.Itoa(sessionIndex)
	sessionTree, err := checkpointTree.Tree(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("session %d not found: %w", sessionIndex, err)
	}

	result := &SessionContent{}

	// Read session-specific metadata
	var agentType agent.AgentType
	if metadataFile, fileErr := sessionTree.File(paths.MetadataFileName); fileErr == nil {
		if content, contentErr := metadataFile.Contents(); contentErr == nil {
			if jsonErr := json.Unmarshal([]byte(content), &result.Metadata); jsonErr == nil {
				agentType = result.Metadata.Agent
			}
		}
	}

	// Read transcript
	if transcript, transcriptErr := readTranscriptFromTree(sessionTree, agentType); transcriptErr == nil && transcript != nil {
		result.Transcript = transcript
	}

	// Read prompts
	if file, fileErr := sessionTree.File(paths.PromptFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			result.Prompts = content
		}
	}

	// Read context
	if file, fileErr := sessionTree.File(paths.ContextFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			result.Context = content
		}
	}

	return result, nil
}

// ReadLatestSessionContent is a convenience method that reads the latest session's content.
// This is equivalent to ReadSessionContent(ctx, checkpointID, len(summary.Sessions)-1).
func (s *GitStore) ReadLatestSessionContent(ctx context.Context, checkpointID id.CheckpointID) (*SessionContent, error) {
	summary, err := s.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}
	if len(summary.Sessions) == 0 {
		return nil, fmt.Errorf("checkpoint has no sessions: %s", checkpointID)
	}

	latestIndex := len(summary.Sessions) - 1
	return s.ReadSessionContent(ctx, checkpointID, latestIndex)
}

// ReadSessionContentByID reads a session's content by its session ID.
// This is useful when you have the session ID but don't know its index within the checkpoint.
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
// Returns an error if no session with the given ID exists in the checkpoint.
func (s *GitStore) ReadSessionContentByID(ctx context.Context, checkpointID id.CheckpointID, sessionID string) (*SessionContent, error) {
	summary, err := s.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}

	// Iterate through sessions to find the one with matching session ID
	for i := range len(summary.Sessions) {
		content, readErr := s.ReadSessionContent(ctx, checkpointID, i)
		if readErr != nil {
			continue
		}
		if content != nil && content.Metadata.SessionID == sessionID {
			return content, nil
		}
	}

	return nil, fmt.Errorf("session %q not found in checkpoint %s", sessionID, checkpointID)
}

// ListCommitted lists all committed checkpoints from the entire/checkpoints/v1 branch.
// Scans sharded paths: <id[:2]>/<id[2:]>/ directories containing metadata.json.
//

func (s *GitStore) ListCommitted(ctx context.Context) ([]CommittedInfo, error) {
	_ = ctx // Reserved for future use

	tree, err := s.getSessionsBranchTree()
	if err != nil {
		return []CommittedInfo{}, nil //nolint:nilerr // No sessions branch means empty list
	}

	var checkpoints []CommittedInfo

	// Scan sharded structure: <2-char-prefix>/<remaining-id>/metadata.json
	for _, bucketEntry := range tree.Entries {
		if bucketEntry.Mode != filemode.Dir {
			continue
		}
		// Bucket should be 2 hex chars
		if len(bucketEntry.Name) != 2 {
			continue
		}

		bucketTree, treeErr := s.repo.TreeObject(bucketEntry.Hash)
		if treeErr != nil {
			continue
		}

		// Each entry in the bucket is the remaining part of the checkpoint ID
		for _, checkpointEntry := range bucketTree.Entries {
			if checkpointEntry.Mode != filemode.Dir {
				continue
			}

			checkpointTree, cpTreeErr := s.repo.TreeObject(checkpointEntry.Hash)
			if cpTreeErr != nil {
				continue
			}

			// Reconstruct checkpoint ID: <bucket><remaining>
			checkpointIDStr := bucketEntry.Name + checkpointEntry.Name
			checkpointID, cpIDErr := id.NewCheckpointID(checkpointIDStr)
			if cpIDErr != nil {
				// Skip invalid checkpoint IDs (shouldn't happen with our own data)
				continue
			}

			info := CommittedInfo{
				CheckpointID: checkpointID,
			}

			// Get details from root metadata file (CheckpointSummary format)
			if metadataFile, fileErr := checkpointTree.File(paths.MetadataFileName); fileErr == nil {
				if content, contentErr := metadataFile.Contents(); contentErr == nil {
					var summary CheckpointSummary
					if err := json.Unmarshal([]byte(content), &summary); err == nil {
						info.CheckpointsCount = summary.CheckpointsCount
						info.FilesTouched = summary.FilesTouched
						info.SessionCount = len(summary.Sessions)

						// Read session metadata from latest session to get Agent, SessionID, CreatedAt
						if len(summary.Sessions) > 0 {
							latestIndex := len(summary.Sessions) - 1
							latestDir := strconv.Itoa(latestIndex)
							if sessionTree, treeErr := checkpointTree.Tree(latestDir); treeErr == nil {
								if sessionMetadataFile, smErr := sessionTree.File(paths.MetadataFileName); smErr == nil {
									if sessionContent, scErr := sessionMetadataFile.Contents(); scErr == nil {
										var sessionMetadata CommittedMetadata
										if json.Unmarshal([]byte(sessionContent), &sessionMetadata) == nil {
											info.Agent = sessionMetadata.Agent
											info.SessionID = sessionMetadata.SessionID
											info.CreatedAt = sessionMetadata.CreatedAt
										}
									}
								}
							}
						}
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

// GetTranscript retrieves the transcript for a specific checkpoint ID.
// Returns the latest session's transcript.
func (s *GitStore) GetTranscript(ctx context.Context, checkpointID id.CheckpointID) ([]byte, error) {
	content, err := s.ReadLatestSessionContent(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	if len(content.Transcript) == 0 {
		return nil, fmt.Errorf("no transcript found for checkpoint: %s", checkpointID)
	}
	return content.Transcript, nil
}

// GetSessionLog retrieves the session transcript and session ID for a checkpoint.
// This is the primary method for looking up session logs by checkpoint ID.
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
// Returns ErrNoTranscript if the checkpoint exists but has no transcript.
func (s *GitStore) GetSessionLog(cpID id.CheckpointID) ([]byte, string, error) {
	content, err := s.ReadLatestSessionContent(context.Background(), cpID)
	if err != nil {
		if errors.Is(err, ErrCheckpointNotFound) {
			return nil, "", ErrCheckpointNotFound
		}
		return nil, "", fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if len(content.Transcript) == 0 {
		return nil, "", ErrNoTranscript
	}
	return content.Transcript, content.Metadata.SessionID, nil
}

// LookupSessionLog is a convenience function that opens the repository and retrieves
// a session log by checkpoint ID. This is the primary entry point for callers that
// don't already have a GitStore instance.
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
// Returns ErrNoTranscript if the checkpoint exists but has no transcript.
func LookupSessionLog(cpID id.CheckpointID) ([]byte, string, error) {
	repo, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, "", fmt.Errorf("failed to open git repository: %w", err)
	}
	store := NewGitStore(repo)
	return store.GetSessionLog(cpID)
}

// UpdateSummary updates the summary field in the latest session's metadata.
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
func (s *GitStore) UpdateSummary(ctx context.Context, checkpointID id.CheckpointID, summary *Summary) error {
	_ = ctx // Reserved for future use

	// Ensure sessions branch exists
	if err := s.ensureSessionsBranch(); err != nil {
		return fmt.Errorf("failed to ensure sessions branch: %w", err)
	}

	// Get current branch tip and flatten tree
	ref, entries, err := s.getSessionsBranchEntries()
	if err != nil {
		return err
	}

	// Read root CheckpointSummary to find the latest session
	basePath := checkpointID.Path() + "/"
	rootMetadataPath := basePath + paths.MetadataFileName
	entry, exists := entries[rootMetadataPath]
	if !exists {
		return ErrCheckpointNotFound
	}

	checkpointSummary, err := s.readSummaryFromBlob(entry.Hash)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint summary: %w", err)
	}

	// Find the latest session's metadata path (0-based indexing)
	latestIndex := len(checkpointSummary.Sessions) - 1
	sessionMetadataPath := fmt.Sprintf("%s%d/%s", basePath, latestIndex, paths.MetadataFileName)
	sessionEntry, exists := entries[sessionMetadataPath]
	if !exists {
		return fmt.Errorf("session metadata not found at %s", sessionMetadataPath)
	}

	// Read and update session metadata
	existingMetadata, err := s.readMetadataFromBlob(sessionEntry.Hash)
	if err != nil {
		return fmt.Errorf("failed to read session metadata: %w", err)
	}

	// Update the summary
	existingMetadata.Summary = summary

	// Write updated session metadata
	metadataJSON, err := jsonutil.MarshalIndentWithNewline(existingMetadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	metadataHash, err := CreateBlobFromContent(s.repo, metadataJSON)
	if err != nil {
		return fmt.Errorf("failed to create metadata blob: %w", err)
	}
	entries[sessionMetadataPath] = object.TreeEntry{
		Name: sessionMetadataPath,
		Mode: filemode.Regular,
		Hash: metadataHash,
	}

	// Build and commit
	newTreeHash, err := BuildTreeFromEntries(s.repo, entries)
	if err != nil {
		return err
	}

	authorName, authorEmail := getGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Update summary for checkpoint %s (session: %s)", checkpointID, existingMetadata.SessionID)
	newCommitHash, err := s.createCommit(newTreeHash, ref.Hash(), commitMsg, authorName, authorEmail)
	if err != nil {
		return err
	}

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	newRef := plumbing.NewHashReference(refName, newCommitHash)
	if err := s.repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to set branch reference: %w", err)
	}

	return nil
}

// ensureSessionsBranch ensures the entire/checkpoints/v1 branch exists.
func (s *GitStore) ensureSessionsBranch() error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	_, err := s.repo.Reference(refName, true)
	if err == nil {
		return nil // Branch exists
	}

	// Create orphan branch with empty tree
	emptyTreeHash, err := BuildTreeFromEntries(s.repo, make(map[string]object.TreeEntry))
	if err != nil {
		return err
	}

	authorName, authorEmail := getGitAuthorFromRepo(s.repo)
	commitHash, err := s.createCommit(emptyTreeHash, plumbing.ZeroHash, "Initialize sessions branch", authorName, authorEmail)
	if err != nil {
		return err
	}

	newRef := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to set branch reference: %w", err)
	}
	return nil
}

// getSessionsBranchTree returns the tree object for the entire/checkpoints/v1 branch.
// Falls back to origin/entire/checkpoints/v1 if the local branch doesn't exist.
func (s *GitStore) getSessionsBranchTree() (*object.Tree, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		// Local branch doesn't exist, try remote-tracking branch
		remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
		ref, err = s.repo.Reference(remoteRefName, true)
		if err != nil {
			return nil, fmt.Errorf("sessions branch not found: %w", err)
		}
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	return tree, nil
}

// CreateBlobFromContent creates a blob object from in-memory content.
// Exported for use by strategy package (session_test.go)
func CreateBlobFromContent(repo *git.Repository, content []byte) (plumbing.Hash, error) {
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))

	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get object writer: %w", err)
	}

	_, err = writer.Write(content)
	if err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, fmt.Errorf("failed to write blob content: %w", err)
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to close blob writer: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store blob object: %w", err)
	}
	return hash, nil
}

// copyMetadataDir copies all files from a directory to the checkpoint path.
// Used to include additional metadata files like task checkpoints, subagent transcripts, etc.
func (s *GitStore) copyMetadataDir(metadataDir, basePath string, entries map[string]object.TreeEntry) error {
	err := filepath.Walk(metadataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// Skip symlinks to prevent reading files outside the metadata directory.
		// A symlink could point to sensitive files (e.g., /etc/passwd) which would
		// then be captured in the checkpoint and stored in git history.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		// Get relative path within metadata dir
		relPath, err := filepath.Rel(metadataDir, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		// Prevent path traversal via symlinks pointing outside the metadata dir
		if strings.HasPrefix(relPath, "..") {
			return fmt.Errorf("path traversal detected: %s", relPath)
		}

		// Create blob from file with secrets redaction
		blobHash, mode, err := createRedactedBlobFromFile(s.repo, path, relPath)
		if err != nil {
			return fmt.Errorf("failed to create blob for %s: %w", path, err)
		}

		// Store at checkpoint path
		fullPath := basePath + relPath
		entries[fullPath] = object.TreeEntry{
			Name: fullPath,
			Mode: mode,
			Hash: blobHash,
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk metadata directory: %w", err)
	}
	return nil
}

// createRedactedBlobFromFile reads a file, applies secrets redaction, and creates a git blob.
// JSONL files get JSONL-aware redaction; all other files get plain string redaction.
func createRedactedBlobFromFile(repo *git.Repository, filePath, treePath string) (plumbing.Hash, filemode.FileMode, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to stat file: %w", err)
	}

	mode := filemode.Regular
	if info.Mode()&0o111 != 0 {
		mode = filemode.Executable
	}

	content, err := os.ReadFile(filePath) //nolint:gosec // filePath comes from walking the metadata directory
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to read file: %w", err)
	}

	// Skip redaction for binary files — they can't contain text secrets and
	// running string replacement on them would corrupt the data.
	isBin, binErr := binary.IsBinary(bytes.NewReader(content))
	if binErr != nil || isBin {
		hash, err := CreateBlobFromContent(repo, content)
		if err != nil {
			return plumbing.ZeroHash, 0, fmt.Errorf("failed to create blob: %w", err)
		}
		return hash, mode, nil
	}

	if strings.HasSuffix(treePath, ".jsonl") {
		content, err = redact.JSONLBytes(content)
		if err != nil {
			return plumbing.ZeroHash, 0, fmt.Errorf("failed to redact secrets: %w", err)
		}
	} else {
		content = redact.Bytes(content)
	}

	hash, err := CreateBlobFromContent(repo, content)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("failed to create blob: %w", err)
	}
	return hash, mode, nil
}

// getGitAuthorFromRepo retrieves the git user.name and user.email from the repository config.
func getGitAuthorFromRepo(repo *git.Repository) (name, email string) {
	// Get repository config (includes local settings)
	cfg, err := repo.Config()
	if err == nil {
		name = cfg.User.Name
		email = cfg.User.Email
	}

	// Provide sensible defaults if git user is not configured
	if name == "" {
		name = "Unknown"
	}
	if email == "" {
		email = "unknown@local"
	}

	return name, email
}

// readTranscriptFromTree reads a transcript from a git tree, handling both chunked and non-chunked formats.
// It checks for chunk files first (.001, .002, etc.), then falls back to the base file.
// The agentType is used for reassembling chunks in the correct format.
func readTranscriptFromTree(tree *object.Tree, agentType agent.AgentType) ([]byte, error) {
	// Collect all transcript-related files
	var chunkFiles []string
	var hasBaseFile bool

	for _, entry := range tree.Entries {
		if entry.Name == paths.TranscriptFileName || entry.Name == paths.TranscriptFileNameLegacy {
			hasBaseFile = true
		}
		// Check for chunk files (full.jsonl.001, full.jsonl.002, etc.)
		if strings.HasPrefix(entry.Name, paths.TranscriptFileName+".") {
			idx := agent.ParseChunkIndex(entry.Name, paths.TranscriptFileName)
			if idx > 0 {
				chunkFiles = append(chunkFiles, entry.Name)
			}
		}
	}

	// If we have chunk files, read and reassemble them
	if len(chunkFiles) > 0 {
		// Sort chunk files by index
		chunkFiles = agent.SortChunkFiles(chunkFiles, paths.TranscriptFileName)

		// Check if base file should be included as chunk 0.
		// NOTE: This assumes the chunking convention where the unsuffixed file
		// (full.jsonl) is chunk 0, and numbered files (.001, .002) are chunks 1+.
		if hasBaseFile {
			chunkFiles = append([]string{paths.TranscriptFileName}, chunkFiles...)
		}

		var chunks [][]byte
		for _, chunkFile := range chunkFiles {
			file, err := tree.File(chunkFile)
			if err != nil {
				logging.Warn(context.Background(), "failed to read transcript chunk file from tree",
					slog.String("chunk_file", chunkFile),
					slog.String("error", err.Error()),
				)
				continue
			}
			content, err := file.Contents()
			if err != nil {
				logging.Warn(context.Background(), "failed to read transcript chunk contents",
					slog.String("chunk_file", chunkFile),
					slog.String("error", err.Error()),
				)
				continue
			}
			chunks = append(chunks, []byte(content))
		}

		if len(chunks) > 0 {
			result, err := agent.ReassembleTranscript(chunks, agentType)
			if err != nil {
				return nil, fmt.Errorf("failed to reassemble transcript: %w", err)
			}
			return result, nil
		}
	}

	// Fall back to reading base file (non-chunked or backwards compatibility)
	if file, err := tree.File(paths.TranscriptFileName); err == nil {
		if content, err := file.Contents(); err == nil {
			return []byte(content), nil
		}
	}

	// Try legacy filename
	if file, err := tree.File(paths.TranscriptFileNameLegacy); err == nil {
		if content, err := file.Contents(); err == nil {
			return []byte(content), nil
		}
	}

	return nil, nil
}

// Author contains author information for a checkpoint.
type Author struct {
	Name  string
	Email string
}

// GetCheckpointAuthor retrieves the author of a checkpoint from the entire/checkpoints/v1 commit history.
// Returns the author of the commit that introduced this checkpoint's metadata.json file.
// Returns empty Author if the checkpoint is not found or the sessions branch doesn't exist.
func (s *GitStore) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error) {
	_ = ctx // Reserved for future use

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return Author{}, nil
	}

	// Path to the checkpoint's metadata file
	metadataPath := checkpointID.Path() + "/" + paths.MetadataFileName

	// Walk commit history looking for the commit that introduced this file
	iter, err := s.repo.Log(&git.LogOptions{
		From:  ref.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return Author{}, nil
	}
	defer iter.Close()

	var author Author
	var foundCommit *object.Commit

	err = iter.ForEach(func(c *object.Commit) error {
		tree, treeErr := c.Tree()
		if treeErr != nil {
			return nil //nolint:nilerr // Skip commits we can't read, continue searching
		}

		_, fileErr := tree.File(metadataPath)
		if fileErr != nil {
			// File doesn't exist in this commit - we've gone past the creation point
			if foundCommit != nil {
				return errStopIteration
			}
			return nil
		}

		// File exists - track it (oldest one with file is the creator)
		foundCommit = c
		author = Author{
			Name:  c.Author.Name,
			Email: c.Author.Email,
		}
		return nil
	})

	// Ignore errStopIteration - it's just for early exit
	if err != nil && !errors.Is(err, errStopIteration) {
		return Author{}, nil
	}

	return author, nil
}
