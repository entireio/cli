package checkpoint

import (
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

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// WriteCommitted writes a committed checkpoint to the entire/sessions branch.
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
	if err := paths.ValidateSessionID(opts.SessionID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}
	if err := paths.ValidateToolUseID(opts.ToolUseID); err != nil {
		return fmt.Errorf("invalid checkpoint options: %w", err)
	}
	if err := paths.ValidateAgentID(opts.AgentID); err != nil {
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
	checkpoint := incrementalCheckpointData{
		Type:      opts.IncrementalType,
		ToolUseID: opts.ToolUseID,
		Timestamp: time.Now().UTC(),
		Data:      opts.IncrementalData,
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

// writeStandardCheckpointEntries writes transcript, prompts, context, metadata.json and any additional files.
// If the checkpoint already exists (from a previous session), archives the existing files to a numbered subfolder.
func (s *GitStore) writeStandardCheckpointEntries(opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry) error {
	// Check if checkpoint already exists (multi-session support)
	var existingMetadata *CommittedMetadata
	metadataPath := basePath + paths.MetadataFileName
	if entry, exists := entries[metadataPath]; exists {
		// Read existing metadata to get session count
		existing, err := s.readMetadataFromBlob(entry.Hash)
		if err == nil {
			existingMetadata = existing
			// Archive existing session files to numbered subfolder
			s.archiveExistingSession(basePath, existingMetadata, entries)
		}
	}

	// Write transcript (from in-memory content or file path)
	if err := s.writeTranscript(opts, basePath, entries); err != nil {
		return err
	}

	// Write prompts
	if len(opts.Prompts) > 0 {
		promptContent := strings.Join(opts.Prompts, "\n\n---\n\n")
		blobHash, err := CreateBlobFromContent(s.repo, []byte(promptContent))
		if err != nil {
			return err
		}
		entries[basePath+paths.PromptFileName] = object.TreeEntry{
			Name: basePath + paths.PromptFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	// Write context
	if len(opts.Context) > 0 {
		blobHash, err := CreateBlobFromContent(s.repo, opts.Context)
		if err != nil {
			return err
		}
		entries[basePath+paths.ContextFileName] = object.TreeEntry{
			Name: basePath + paths.ContextFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	// Copy additional metadata files from directory if specified
	if opts.MetadataDir != "" {
		if err := s.copyMetadataDir(opts.MetadataDir, basePath, entries); err != nil {
			return fmt.Errorf("failed to copy metadata directory: %w", err)
		}
	}

	// Write metadata.json (with merged info if existing metadata present)
	return s.writeMetadataJSON(opts, basePath, entries, existingMetadata)
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

// writeMetadataJSON writes the metadata.json file for the checkpoint.
// If existingMetadata is provided, merges session info from the previous session(s).
func (s *GitStore) writeMetadataJSON(opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry, existingMetadata *CommittedMetadata) error {
	metadata := CommittedMetadata{
		CheckpointID:           opts.CheckpointID,
		SessionID:              opts.SessionID,
		Strategy:               opts.Strategy,
		CreatedAt:              time.Now(),
		Branch:                 opts.Branch,
		CheckpointsCount:       opts.CheckpointsCount,
		FilesTouched:           opts.FilesTouched,
		Agent:                  opts.Agent,
		IsTask:                 opts.IsTask,
		ToolUseID:              opts.ToolUseID,
		SessionCount:           1,
		SessionIDs:             []string{opts.SessionID},
		TranscriptUUIDAtStart:  opts.TranscriptUUIDAtStart,
		TranscriptLinesAtStart: opts.TranscriptLinesAtStart,
		TokenUsage:             opts.TokenUsage,
	}

	// Merge with existing metadata if present (multi-session checkpoint)
	if existingMetadata != nil {
		// Get existing session count (default to 1 for backwards compat)
		existingCount := existingMetadata.SessionCount
		if existingCount == 0 {
			existingCount = 1
		}
		metadata.SessionCount = existingCount + 1

		// Merge session IDs
		existingIDs := existingMetadata.SessionIDs
		if len(existingIDs) == 0 {
			// Backwards compat: old metadata only had SessionID
			existingIDs = []string{existingMetadata.SessionID}
		}
		metadata.SessionIDs = append(metadata.SessionIDs[:0], existingIDs...)
		metadata.SessionIDs = append(metadata.SessionIDs, opts.SessionID)

		// Merge files touched (deduplicated)
		metadata.FilesTouched = mergeFilesTouched(existingMetadata.FilesTouched, opts.FilesTouched)

		// Sum checkpoint counts
		metadata.CheckpointsCount = existingMetadata.CheckpointsCount + opts.CheckpointsCount
	}

	metadataJSON, err := jsonutil.MarshalIndentWithNewline(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
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
	blob, err := s.repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get blob: %w", err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to get blob reader: %w", err)
	}
	defer reader.Close()

	var metadata CommittedMetadata
	if err := json.NewDecoder(reader).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}

	return &metadata, nil
}

// archiveExistingSession moves existing session files to a numbered subfolder.
// The subfolder number is based on the current session count (so first archived session goes to "1/").
func (s *GitStore) archiveExistingSession(basePath string, existingMetadata *CommittedMetadata, entries map[string]object.TreeEntry) {
	// Determine archive folder number
	sessionCount := existingMetadata.SessionCount
	if sessionCount == 0 {
		sessionCount = 1 // backwards compat
	}
	archivePath := fmt.Sprintf("%s%d/", basePath, sessionCount)

	// Files to archive (standard checkpoint files at basePath, excluding tasks/ subfolder)
	filesToArchive := []string{
		paths.MetadataFileName,
		paths.TranscriptFileName,
		paths.PromptFileName,
		paths.ContextFileName,
		paths.ContentHashFileName,
	}

	// Also include transcript chunk files (full.jsonl.001, full.jsonl.002, etc.)
	chunkPrefix := basePath + paths.TranscriptFileName + "."
	for srcPath := range entries {
		if strings.HasPrefix(srcPath, chunkPrefix) {
			chunkSuffix := strings.TrimPrefix(srcPath, basePath+paths.TranscriptFileName)
			if idx := agent.ParseChunkIndex(paths.TranscriptFileName+chunkSuffix, paths.TranscriptFileName); idx > 0 {
				filesToArchive = append(filesToArchive, paths.TranscriptFileName+chunkSuffix)
			}
		}
	}

	// Move each file to archive folder
	for _, filename := range filesToArchive {
		srcPath := basePath + filename
		if entry, exists := entries[srcPath]; exists {
			// Add to archive location
			dstPath := archivePath + filename
			entries[dstPath] = object.TreeEntry{
				Name: dstPath,
				Mode: entry.Mode,
				Hash: entry.Hash,
			}
			// Remove from original location (will be overwritten by new session)
			delete(entries, srcPath)
		}
	}
}

// readArchivedSessions reads transcript data from archived session subfolders (1/, 2/, etc.).
// Returns sessions ordered by folder index (oldest first).
func (s *GitStore) readArchivedSessions(checkpointTree *object.Tree, sessionCount int, agentType string) []ArchivedSession {
	var archived []ArchivedSession

	// Archived sessions are in numbered folders: 1/, 2/, etc.
	// The most recent session is at the root level (not archived).
	// Session count N means there are N-1 archived sessions.
	for i := 1; i < sessionCount; i++ {
		folderName := strconv.Itoa(i)

		// Try to get the archived session subtree
		subTree, err := checkpointTree.Tree(folderName)
		if err != nil {
			continue // Folder doesn't exist, skip
		}

		session := ArchivedSession{
			FolderIndex: i,
		}

		// Read metadata to get session ID
		if metadataFile, fileErr := subTree.File(paths.MetadataFileName); fileErr == nil {
			if content, contentErr := metadataFile.Contents(); contentErr == nil {
				var metadata CommittedMetadata
				if jsonErr := json.Unmarshal([]byte(content), &metadata); jsonErr == nil {
					session.SessionID = metadata.SessionID
				}
			}
		}

		// Read transcript (handles both chunked and non-chunked formats)
		if transcript, err := readTranscriptFromTree(subTree, agentType); err == nil && transcript != nil {
			session.Transcript = transcript
		}

		// Read prompts
		if file, fileErr := subTree.File(paths.PromptFileName); fileErr == nil {
			if content, contentErr := file.Contents(); contentErr == nil {
				session.Prompts = content
			}
		}

		// Only add if we got a transcript
		if len(session.Transcript) > 0 {
			archived = append(archived, session)
		}
	}

	return archived
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

// ReadCommitted reads a committed checkpoint by ID from the entire/sessions branch.
// Returns nil, nil if the checkpoint doesn't exist.
//

func (s *GitStore) ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*ReadCommittedResult, error) {
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

	result := &ReadCommittedResult{}

	// Read metadata.json
	if metadataFile, fileErr := checkpointTree.File(paths.MetadataFileName); fileErr == nil {
		if content, contentErr := metadataFile.Contents(); contentErr == nil {
			//nolint:errcheck,gosec // Best-effort parsing, defaults are fine
			json.Unmarshal([]byte(content), &result.Metadata)
			result.Metadata.Strategy = trailers.NormalizeStrategyName(result.Metadata.Strategy)
		}
	}

	// Read transcript (handles both chunked and non-chunked formats)
	if transcript, err := readTranscriptFromTree(checkpointTree, result.Metadata.Agent); err == nil && transcript != nil {
		result.Transcript = transcript
	}

	// Read prompts
	if file, fileErr := checkpointTree.File(paths.PromptFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			result.Prompts = content
		}
	}

	// Read context
	if file, fileErr := checkpointTree.File(paths.ContextFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			result.Context = content
		}
	}

	// Read archived sessions if this is a multi-session checkpoint
	if result.Metadata.SessionCount > 1 {
		result.ArchivedSessions = s.readArchivedSessions(checkpointTree, result.Metadata.SessionCount, result.Metadata.Agent)
	}

	return result, nil
}

// ListCommitted lists all committed checkpoints from the entire/sessions branch.
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

			// Get details from metadata file
			if metadataFile, fileErr := checkpointTree.File(paths.MetadataFileName); fileErr == nil {
				if content, contentErr := metadataFile.Contents(); contentErr == nil {
					var metadata CommittedMetadata
					if err := json.Unmarshal([]byte(content), &metadata); err == nil {
						info.SessionID = metadata.SessionID
						info.CreatedAt = metadata.CreatedAt
						info.CheckpointsCount = metadata.CheckpointsCount
						info.FilesTouched = metadata.FilesTouched
						info.Agent = metadata.Agent
						info.IsTask = metadata.IsTask
						info.ToolUseID = metadata.ToolUseID
						info.SessionCount = metadata.SessionCount
						info.SessionIDs = metadata.SessionIDs
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
func (s *GitStore) GetTranscript(ctx context.Context, checkpointID id.CheckpointID) ([]byte, error) {
	result, err := s.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("checkpoint not found: %s", checkpointID)
	}
	if len(result.Transcript) == 0 {
		return nil, fmt.Errorf("no transcript found for checkpoint: %s", checkpointID)
	}
	return result.Transcript, nil
}

// GetSessionLog retrieves the session transcript and session ID for a checkpoint.
// This is the primary method for looking up session logs by checkpoint ID.
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
// Returns ErrNoTranscript if the checkpoint exists but has no transcript.
func (s *GitStore) GetSessionLog(cpID id.CheckpointID) ([]byte, string, error) {
	result, err := s.ReadCommitted(context.Background(), cpID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if result == nil {
		return nil, "", ErrCheckpointNotFound
	}
	if len(result.Transcript) == 0 {
		return nil, "", ErrNoTranscript
	}
	return result.Transcript, result.Metadata.SessionID, nil
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

// ensureSessionsBranch ensures the entire/sessions branch exists.
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

// getSessionsBranchTree returns the tree object for the entire/sessions branch.
func (s *GitStore) getSessionsBranchTree() (*object.Tree, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("sessions branch not found: %w", err)
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

		// Create blob from file
		blobHash, mode, err := createBlobFromFile(s.repo, path)
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
func readTranscriptFromTree(tree *object.Tree, agentType string) ([]byte, error) {
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
