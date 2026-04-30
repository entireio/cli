package checkpoint

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// WriteCommitted writes a committed checkpoint to both v2 refs:
//   - /main: metadata and prompts (no raw transcript or content hash)
//   - /full/current: raw transcript + content hash (replaces previous content)
//
// This is the public entry point for v2 dual-writes. The session index is
// determined from the /main ref and passed to the /full/current write to
// keep both refs consistent.
func (s *V2GitStore) WriteCommitted(ctx context.Context, opts WriteCommittedOptions) error {
	// Validate upfront before any writes to avoid partial ref updates
	if err := validateWriteOpts(opts); err != nil {
		return err
	}

	sessionIndex, err := s.writeCommittedMain(ctx, opts)
	if err != nil {
		return fmt.Errorf("v2 /main write failed: %w", err)
	}

	if err := s.writeCommittedFullTranscript(ctx, opts, sessionIndex); err != nil {
		return fmt.Errorf("v2 /full/current write failed: %w", err)
	}

	return nil
}

// UpdateCommitted replaces the prompts and/or transcript for an existing v2
// checkpoint. Called at stop time to finalize checkpoints with the complete
// session transcript.
//
// On /main: replaces prompts and compact transcript (if provided).
// On /full/*: replaces the raw transcript where the session artifacts already
// live, or writes to /full/current if the session has no full artifacts yet.
//
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist on /main.
func (s *V2GitStore) UpdateCommitted(ctx context.Context, opts UpdateCommittedOptions) error {
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid update options: checkpoint ID is required")
	}

	sessionIndex, err := s.updateCommittedMain(ctx, opts)
	if err != nil {
		return fmt.Errorf("v2 /main update failed: %w", err)
	}

	if opts.Transcript.Len() > 0 {
		if err := s.updateCommittedFullTranscript(ctx, opts, sessionIndex); err != nil {
			return fmt.Errorf("v2 /full/* update failed: %w", err)
		}
	}

	return nil
}

// fullSessionArtifacts describes where a checkpoint session's raw transcript
// artifacts live across the v2 /full/* refs.
type fullSessionArtifacts struct {
	RefName       plumbing.ReferenceName
	Found         bool
	HasTranscript bool
	HasHash       bool
}

// HasFullSessionArtifacts reports whether the raw transcript and content hash
// for a checkpoint session exist in any local v2 /full/* ref.
func (s *V2GitStore) HasFullSessionArtifacts(checkpointID id.CheckpointID, sessionIndex int) (bool, error) {
	artifacts, err := s.findFullSessionArtifacts(checkpointID, sessionIndex)
	if err != nil {
		return false, err
	}
	return artifacts.Found && artifacts.HasTranscript && artifacts.HasHash, nil
}

func (s *V2GitStore) findFullSessionArtifacts(checkpointID id.CheckpointID, sessionIndex int) (fullSessionArtifacts, error) {
	refNames, err := s.fullRefSearchOrder()
	if err != nil {
		return fullSessionArtifacts{}, err
	}

	var firstFound fullSessionArtifacts
	for _, refName := range refNames {
		artifacts, inspectErr := s.inspectFullSessionArtifacts(refName, checkpointID, sessionIndex)
		if inspectErr != nil {
			return fullSessionArtifacts{}, inspectErr
		}
		if !artifacts.Found {
			continue
		}
		if artifacts.HasTranscript && artifacts.HasHash {
			return artifacts, nil
		}
		if !firstFound.Found {
			firstFound = artifacts
		}
	}

	if firstFound.Found {
		return firstFound, nil
	}

	return fullSessionArtifacts{}, nil
}

func (s *V2GitStore) fullRefSearchOrder() ([]plumbing.ReferenceName, error) {
	refNames := []plumbing.ReferenceName{plumbing.ReferenceName(paths.V2FullCurrentRefName)}

	archived, err := s.ListArchivedGenerations()
	if err != nil {
		return nil, err
	}
	for i := len(archived) - 1; i >= 0; i-- {
		refNames = append(refNames, plumbing.ReferenceName(paths.V2FullRefPrefix+archived[i]))
	}

	return refNames, nil
}

func (s *V2GitStore) inspectFullSessionArtifacts(refName plumbing.ReferenceName, checkpointID id.CheckpointID, sessionIndex int) (fullSessionArtifacts, error) {
	_, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return fullSessionArtifacts{}, nil
		}
		return fullSessionArtifacts{}, err
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return fullSessionArtifacts{}, fmt.Errorf("failed to read %s tree: %w", refName, err)
	}

	sessionPath := fmt.Sprintf("%s/%d", checkpointID.Path(), sessionIndex)
	sessionTree, err := rootTree.Tree(sessionPath)
	if err != nil {
		if errors.Is(err, object.ErrDirectoryNotFound) {
			return fullSessionArtifacts{}, nil
		}
		return fullSessionArtifacts{}, fmt.Errorf("failed to read %s session tree %s: %w", refName, sessionPath, err)
	}

	artifacts := fullSessionArtifacts{RefName: refName, Found: true}
	for _, entry := range sessionTree.Entries {
		switch {
		case entry.Name == paths.V2RawTranscriptFileName:
			artifacts.HasTranscript = true
		case strings.HasPrefix(entry.Name, paths.V2RawTranscriptFileName+"."):
			artifacts.HasTranscript = true
		case entry.Name == paths.V2RawTranscriptHashFileName:
			artifacts.HasHash = true
		}
	}

	return artifacts, nil
}

// updateCommittedMain updates prompts and compact transcript on the /main ref for an existing checkpoint.
// Returns the session index for coordination with /full/current.
func (s *V2GitStore) updateCommittedMain(ctx context.Context, opts UpdateCommittedOptions) (int, error) {
	refName := plumbing.ReferenceName(paths.V2MainRefName)
	parentHash, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return 0, ErrCheckpointNotFound
	}

	basePath := opts.CheckpointID.Path() + "/"
	checkpointPath := opts.CheckpointID.Path()

	entries, err := s.gs.flattenCheckpointEntries(rootTreeHash, checkpointPath)
	if err != nil {
		return 0, err
	}

	rootMetadataPath := basePath + paths.MetadataFileName
	entry, exists := entries[rootMetadataPath]
	if !exists {
		return 0, ErrCheckpointNotFound
	}

	summary, err := readJSONFromBlob[CheckpointSummary](s.repo, entry.Hash)
	if err != nil {
		return 0, fmt.Errorf("failed to read checkpoint summary: %w", err)
	}
	if len(summary.Sessions) == 0 {
		return 0, ErrCheckpointNotFound
	}

	// Find session index by ID, fall back to latest
	sessionIndex := s.gs.findSessionIndex(ctx, basePath, summary, entries, opts.SessionID)
	if sessionIndex >= len(summary.Sessions) {
		// findSessionIndex returns next-available when not found; fall back to latest
		sessionIndex = len(summary.Sessions) - 1
		logging.Debug(ctx, "v2 UpdateCommitted: session ID not found, falling back to latest",
			slog.String("session_id", opts.SessionID),
			slog.String("checkpoint_id", string(opts.CheckpointID)),
			slog.Int("fallback_index", sessionIndex),
		)
	}

	sessionPath := fmt.Sprintf("%s%d/", basePath, sessionIndex)

	if len(opts.Prompts) > 0 {
		promptContent := redact.String(JoinPrompts(opts.Prompts))
		blobHash, err := CreateBlobFromContent(s.repo, []byte(promptContent))
		if err != nil {
			return 0, fmt.Errorf("failed to create prompt blob: %w", err)
		}
		entries[sessionPath+paths.PromptFileName] = object.TreeEntry{
			Name: sessionPath + paths.PromptFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	// Replace compact transcript if provided
	if len(opts.CompactTranscript) > 0 {
		blobHash, err := CreateBlobFromContent(s.repo, opts.CompactTranscript)
		if err != nil {
			return 0, fmt.Errorf("failed to create compact transcript blob: %w", err)
		}
		entries[sessionPath+paths.CompactTranscriptFileName] = object.TreeEntry{
			Name: sessionPath + paths.CompactTranscriptFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		}

		if err := s.writeCompactTranscriptHash(opts.CompactTranscript, sessionPath, entries); err != nil {
			return 0, fmt.Errorf("failed to write compact transcript hash: %w", err)
		}

		// Keep root checkpoint summary in sync with compact artifact paths.
		if sessionIndex >= 0 && sessionIndex < len(summary.Sessions) {
			summary.Sessions[sessionIndex].Transcript = "/" + sessionPath + paths.CompactTranscriptFileName
			summary.Sessions[sessionIndex].ContentHash = "/" + sessionPath + paths.CompactTranscriptHashFileName

			summaryBytes, err := jsonutil.MarshalIndentWithNewline(summary, "", "  ")
			if err != nil {
				return 0, fmt.Errorf("failed to marshal checkpoint summary: %w", err)
			}
			summaryHash, err := CreateBlobFromContent(s.repo, summaryBytes)
			if err != nil {
				return 0, fmt.Errorf("failed to create checkpoint summary blob: %w", err)
			}
			entries[rootMetadataPath] = object.TreeEntry{
				Name: rootMetadataPath,
				Mode: filemode.Regular,
				Hash: summaryHash,
			}
		}
	}

	newTreeHash, err := s.gs.spliceCheckpointSubtree(ctx, rootTreeHash, opts.CheckpointID, basePath, entries)
	if err != nil {
		return 0, err
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Finalize checkpoint: %s\n", opts.CheckpointID)
	if err := s.updateRef(ctx, refName, newTreeHash, parentHash, commitMsg, authorName, authorEmail); err != nil {
		return 0, err
	}

	return sessionIndex, nil
}

// updateCommittedFullTranscript replaces the transcript for a specific checkpoint
// on the /full/* ref where that checkpoint session already lives, while
// preserving other checkpoints' transcripts in the tree. If the session has no
// full-transcript artifacts yet, it writes to /full/current.
func (s *V2GitStore) updateCommittedFullTranscript(ctx context.Context, opts UpdateCommittedOptions, sessionIndex int) error {
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)

	existing, findErr := s.findFullSessionArtifacts(opts.CheckpointID, sessionIndex)
	if findErr != nil {
		return findErr
	}
	if existing.Found {
		refName = existing.RefName
	}

	if refName == plumbing.ReferenceName(paths.V2FullCurrentRefName) {
		if err := s.ensureRef(ctx, refName); err != nil {
			return fmt.Errorf("failed to ensure /full/current ref: %w", err)
		}
	}

	parentHash, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return err
	}

	basePath := opts.CheckpointID.Path() + "/"
	checkpointPath := opts.CheckpointID.Path()
	sessionPath := fmt.Sprintf("%s%d/", basePath, sessionIndex)

	// Read existing entries and replace transcript for this checkpoint only
	entries, err := s.gs.flattenCheckpointEntries(rootTreeHash, checkpointPath)
	if err != nil {
		return err
	}

	// Ignore precompute if invariants are violated — fall back to fresh chunking.
	precomputed := opts.PrecomputedBlobs
	if precomputed != nil && !precomputed.isUsable() {
		precomputed = nil
	}

	// Short-circuit: if the existing raw_transcript_hash.txt already matches
	// the new transcript's sha256, the existing chunk entries represent the
	// same content — preserve them and skip chunking + zlib.
	rawTranscriptPath := sessionPath + paths.V2RawTranscriptFileName
	rawHashPath := sessionPath + paths.V2RawTranscriptHashFileName
	var newContentHash string
	if precomputed != nil {
		newContentHash = precomputed.ContentHash
	} else {
		newContentHash = fmt.Sprintf("sha256:%x", sha256.Sum256(opts.Transcript.Bytes()))
	}
	if existing, ok := entries[rawHashPath]; ok {
		if blob, err := s.repo.BlobObject(existing.Hash); err == nil {
			if rdr, rerr := blob.Reader(); rerr == nil {
				existingHash, readErr := io.ReadAll(rdr)
				_ = rdr.Close()
				if readErr == nil && string(existingHash) == newContentHash {
					// Content unchanged — skip tree surgery and ref advance to
					// avoid a no-op commit on /full/current. The existing ref
					// already references the correct tree.
					return nil
				}
			}
		}
	}

	// Clear existing transcript artifacts for this session path before writing new ones.
	// Preserve non-transcript metadata under the same session (e.g., tasks/*).
	for key := range entries {
		switch {
		case key == rawTranscriptPath:
			delete(entries, key)
		case strings.HasPrefix(key, rawTranscriptPath+"."):
			delete(entries, key)
		case key == rawHashPath:
			delete(entries, key)
		}
	}

	if err := s.writeTranscriptBlobs(ctx, opts.Transcript, opts.Agent, precomputed, sessionPath, entries); err != nil {
		return err
	}

	if err := s.writeContentHashFromPrecompute(newContentHash, precomputed, sessionPath, entries); err != nil {
		return err
	}

	// Splice into existing root tree (preserves other checkpoints' transcripts)
	newTreeHash, err := s.gs.spliceCheckpointSubtree(ctx, rootTreeHash, opts.CheckpointID, basePath, entries)
	if err != nil {
		return err
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Finalize checkpoint: %s\n", opts.CheckpointID)
	if err := s.updateRef(ctx, refName, newTreeHash, parentHash, commitMsg, authorName, authorEmail); err != nil {
		return err
	}

	if refName == plumbing.ReferenceName(paths.V2FullCurrentRefName) {
		s.rotateCurrentIfNeeded(ctx, newTreeHash)
	}

	return nil
}

// writeCommittedMain writes metadata entries to the /main ref.
// This includes session metadata and prompts — but NOT the raw transcript
// (raw_transcript) or content hash (raw_transcript_hash.txt), which go to /full/current.
// Returns the session index used, so the caller can pass it to writeCommittedFullTranscript.
func (s *V2GitStore) writeCommittedMain(ctx context.Context, opts WriteCommittedOptions) (int, error) {
	refName := plumbing.ReferenceName(paths.V2MainRefName)
	if err := s.ensureRef(ctx, refName); err != nil {
		return 0, fmt.Errorf("failed to ensure /main ref: %w", err)
	}

	parentHash, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return 0, err
	}

	basePath := opts.CheckpointID.Path() + "/"
	checkpointPath := opts.CheckpointID.Path()

	// Read existing entries at this checkpoint's shard path
	entries, err := s.gs.flattenCheckpointEntries(rootTreeHash, checkpointPath)
	if err != nil {
		return 0, err
	}

	// Build main session entries (metadata, prompts — no transcript or content hash)
	sessionIndex, err := s.writeMainCheckpointEntries(ctx, opts, basePath, entries)
	if err != nil {
		return 0, err
	}

	// Splice entries into root tree
	newTreeHash, err := s.gs.spliceCheckpointSubtree(ctx, rootTreeHash, opts.CheckpointID, basePath, entries)
	if err != nil {
		return 0, err
	}

	commitMsg := fmt.Sprintf("Checkpoint: %s\n", opts.CheckpointID)
	if err := s.updateRef(ctx, refName, newTreeHash, parentHash, commitMsg, opts.AuthorName, opts.AuthorEmail); err != nil {
		return 0, err
	}
	return sessionIndex, nil
}

// writeMainCheckpointEntries orchestrates writing session data to the /main ref.
// It mirrors GitStore.writeStandardCheckpointEntries but excludes raw transcript blobs.
// Returns the session index used, for coordination with writeCommittedFullTranscript.
func (s *V2GitStore) writeMainCheckpointEntries(ctx context.Context, opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry) (int, error) {
	// Read existing summary to get current session count
	var existingSummary *CheckpointSummary
	metadataPath := basePath + paths.MetadataFileName
	if entry, exists := entries[metadataPath]; exists {
		existing, err := readJSONFromBlob[CheckpointSummary](s.repo, entry.Hash)
		if err == nil {
			existingSummary = existing
		}
	}

	// Determine session index
	sessionIndex := s.gs.findSessionIndex(ctx, basePath, existingSummary, entries, opts.SessionID)

	// Refuse if slot 0 already holds metadata for a DIFFERENT session ID.
	// Mirrors GitStore.writeStandardCheckpointEntries: findSessionIndex only
	// picks slot 0 when existingSummary is nil or when the summary claims slot 0
	// belongs to us, so the actual tree holding session-0 metadata for someone
	// else is a corruption / stale-summary shape. Read BEFORE
	// writeMainSessionToSubdirectory clears the subtree, or we'd only ever see
	// our own write.
	if sessionIndex == 0 {
		if entry, exists := entries[fmt.Sprintf("%s0/%s", basePath, paths.MetadataFileName)]; exists {
			if existingMeta, readErr := s.gs.readMetadataFromBlob(entry.Hash); readErr == nil && existingMeta.SessionID != opts.SessionID {
				logging.Error(ctx, "refusing v2 checkpoint write: session 0 holds a different sessionID",
					slog.String("checkpoint_id", opts.CheckpointID.String()),
					slog.String("existing_session_id", existingMeta.SessionID),
					slog.String("write_session_id", opts.SessionID),
					slog.Bool("existing_summary_nil", existingSummary == nil))
				return 0, fmt.Errorf(
					"refusing to overwrite session 0 of checkpoint %s: existing session ID %q differs from write session ID %q. The v2 checkpoint tree is inconsistent (session 0 belongs to a different session than this write claims). No automated repair exists for this shape — please report it along with the output of `git ls-tree %s %s/`",
					opts.CheckpointID, existingMeta.SessionID, opts.SessionID, paths.V2MainRefName, opts.CheckpointID.Path(),
				)
			}
		}
	}

	// Write session files (metadata and prompts — no transcript or content hash)
	sessionPath := fmt.Sprintf("%s%d/", basePath, sessionIndex)
	sessionFilePaths, err := s.writeMainSessionToSubdirectory(opts, sessionPath, entries)
	if err != nil {
		return 0, err
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

	// Write root CheckpointSummary
	if err := s.gs.writeCheckpointSummary(opts, basePath, entries, sessions); err != nil {
		return 0, err
	}
	return sessionIndex, nil
}

// writeMainSessionToSubdirectory writes a single session's metadata, prompts,
// and compact transcript to a session subdirectory (0/, 1/, 2/, … indexed by
// session order within the checkpoint). The raw transcript (raw_transcript) and its
// content hash (raw_transcript_hash.txt) go to /full/current, not here.
func (s *V2GitStore) writeMainSessionToSubdirectory(opts WriteCommittedOptions, sessionPath string, entries map[string]object.TreeEntry) (SessionFilePaths, error) {
	filePaths := SessionFilePaths{}

	// Clear existing entries at this session path
	for key := range entries {
		if strings.HasPrefix(key, sessionPath) {
			delete(entries, key)
		}
	}

	// Write prompts
	if len(opts.Prompts) > 0 {
		promptContent := redact.String(JoinPrompts(opts.Prompts))
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

	// Write compact transcript (transcript.jsonl) + hash if provided
	if len(opts.CompactTranscript) > 0 {
		blobHash, err := CreateBlobFromContent(s.repo, opts.CompactTranscript)
		if err != nil {
			return filePaths, fmt.Errorf("failed to create compact transcript blob: %w", err)
		}
		entries[sessionPath+paths.CompactTranscriptFileName] = object.TreeEntry{
			Name: sessionPath + paths.CompactTranscriptFileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
		filePaths.Transcript = "/" + sessionPath + paths.CompactTranscriptFileName

		if err := s.writeCompactTranscriptHash(opts.CompactTranscript, sessionPath, entries); err != nil {
			return filePaths, fmt.Errorf("failed to write compact transcript hash: %w", err)
		}
		filePaths.ContentHash = "/" + sessionPath + paths.CompactTranscriptHashFileName
	}

	// Write session metadata
	sessionMetadata := CommittedMetadata{
		CheckpointID:                opts.CheckpointID,
		SessionID:                   opts.SessionID,
		Strategy:                    opts.Strategy,
		CreatedAt:                   time.Now().UTC(),
		Branch:                      opts.Branch,
		CheckpointsCount:            opts.CheckpointsCount,
		FilesTouched:                opts.FilesTouched,
		Agent:                       opts.Agent,
		Model:                       opts.Model,
		TurnID:                      opts.TurnID,
		IsTask:                      opts.IsTask,
		ToolUseID:                   opts.ToolUseID,
		TranscriptIdentifierAtStart: opts.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   opts.CompactTranscriptStart,
		TokenUsage:                  opts.TokenUsage,
		SessionMetrics:              opts.SessionMetrics,
		InitialAttribution:          opts.InitialAttribution,
		PromptAttributions:          opts.PromptAttributionsJSON,
		Summary:                     redactSummary(opts.Summary),
		CLIVersion:                  versioninfo.Version,
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

// writeCompactTranscriptHash computes and writes the SHA-256 hash of the compact transcript.
func (s *V2GitStore) writeCompactTranscriptHash(compactTranscript []byte, sessionPath string, entries map[string]object.TreeEntry) error {
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256(compactTranscript))
	blobHash, err := CreateBlobFromContent(s.repo, []byte(hash))
	if err != nil {
		return err
	}
	entries[sessionPath+paths.CompactTranscriptHashFileName] = object.TreeEntry{
		Name: sessionPath + paths.CompactTranscriptHashFileName,
		Mode: filemode.Regular,
		Hash: blobHash,
	}
	return nil
}

// writeCommittedFullTranscript writes the raw transcript to the /full/current ref.
// Transcripts accumulate across checkpoints — each write splices into the existing
// tree. Generation metadata (generation.json) at the tree root is updated on every
// write with the new checkpoint ID and timestamps.
//
// sessionIndex is the session slot (0-based), determined by the caller to stay
// consistent with the /main ref's session numbering.
// This is a no-op if opts.Transcript is empty (and opts.TranscriptPath is unset).
func (s *V2GitStore) writeCommittedFullTranscript(ctx context.Context, opts WriteCommittedOptions, sessionIndex int) error {
	transcript := opts.Transcript

	// TranscriptPath fallback: data read from disk is an untrusted source,
	// so we redact it here. The in-memory path (opts.Transcript) is already
	// pre-redacted by the caller.
	if transcript.Len() == 0 && opts.TranscriptPath != "" {
		rawData, readErr := os.ReadFile(opts.TranscriptPath)
		if readErr != nil {
			rawData = nil
		}
		if len(rawData) > 0 {
			redacted, redactErr := redact.JSONLBytes(rawData)
			if redactErr != nil {
				return fmt.Errorf("failed to redact transcript from file: %w", redactErr)
			}
			transcript = redacted
		}
	}
	if transcript.Len() == 0 {
		return nil // No transcript to write
	}

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	if err := s.ensureRef(ctx, refName); err != nil {
		return fmt.Errorf("failed to ensure /full/current ref: %w", err)
	}

	parentHash, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return err
	}

	basePath := opts.CheckpointID.Path() + "/"
	checkpointPath := opts.CheckpointID.Path()
	sessionPath := fmt.Sprintf("%s%d/", basePath, sessionIndex)

	// Read existing entries at this checkpoint's shard path
	entries, err := s.gs.flattenCheckpointEntries(rootTreeHash, checkpointPath)
	if err != nil {
		return err
	}

	// Clear existing entries at this session path before writing new ones
	for key := range entries {
		if strings.HasPrefix(key, sessionPath) {
			delete(entries, key)
		}
	}

	if err := s.writeTranscriptBlobs(ctx, transcript, opts.Agent, nil, sessionPath, entries); err != nil {
		return err
	}

	contentHash := fmt.Sprintf("sha256:%x", sha256.Sum256(transcript.Bytes()))
	if err := s.writeContentHashFromPrecompute(contentHash, nil, sessionPath, entries); err != nil {
		return err
	}

	// Splice checkpoint data into the root tree (preserves other checkpoints' transcripts)
	newTreeHash, err := s.gs.spliceCheckpointSubtree(ctx, rootTreeHash, opts.CheckpointID, basePath, entries)
	if err != nil {
		return err
	}

	commitMsg := fmt.Sprintf("Checkpoint: %s\n", opts.CheckpointID)
	if err := s.updateRef(ctx, refName, newTreeHash, parentHash, commitMsg, opts.AuthorName, opts.AuthorEmail); err != nil {
		return err
	}

	s.rotateCurrentIfNeeded(ctx, newTreeHash)
	return nil
}

func (s *V2GitStore) rotateCurrentIfNeeded(ctx context.Context, treeHash plumbing.Hash) {
	checkpointCount, countErr := s.CountCheckpointsInTree(treeHash)
	if countErr != nil {
		logging.Warn(ctx, "failed to count checkpoints for rotation check",
			slog.String("error", countErr.Error()),
		)
		return
	}
	if checkpointCount < s.maxCheckpoints() {
		return
	}
	if rotErr := s.rotateGeneration(ctx); rotErr != nil {
		logging.Warn(ctx, "generation rotation failed",
			slog.String("error", rotErr.Error()),
			slog.Int("checkpoint_count", checkpointCount),
		)
		// Non-fatal: rotation failure doesn't invalidate the write
	}
}

// writeTranscriptBlobs writes pre-redacted, chunked transcript blobs to entries.
// When precomputed is non-nil, reuses its chunk blob hashes and skips both
// ChunkTranscript and CreateBlobFromContent.
func (s *V2GitStore) writeTranscriptBlobs(ctx context.Context, transcript redact.RedactedBytes, agentType types.AgentType, precomputed *PrecomputedTranscriptBlobs, sessionPath string, entries map[string]object.TreeEntry) error {
	var chunkHashes []plumbing.Hash
	if precomputed != nil {
		chunkHashes = precomputed.ChunkHashes
	} else {
		chunks, err := chunkTranscript(ctx, transcript.Bytes(), agentType)
		if err != nil {
			return fmt.Errorf("failed to chunk transcript: %w", err)
		}
		chunkHashes = make([]plumbing.Hash, len(chunks))
		for i, chunk := range chunks {
			h, err := CreateBlobFromContent(s.repo, chunk)
			if err != nil {
				return err
			}
			chunkHashes[i] = h
		}
	}

	for i, blobHash := range chunkHashes {
		chunkPath := sessionPath + agent.ChunkFileName(paths.V2RawTranscriptFileName, i)
		entries[chunkPath] = object.TreeEntry{
			Name: chunkPath,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	return nil
}

// writeContentHashFromPrecompute writes the content-hash blob for the given
// transcript hash. When precomputed is non-nil, reuses its ContentHashBlob
// hash; otherwise creates a fresh blob.
func (s *V2GitStore) writeContentHashFromPrecompute(contentHash string, precomputed *PrecomputedTranscriptBlobs, sessionPath string, entries map[string]object.TreeEntry) error {
	var hashBlob plumbing.Hash
	if precomputed != nil {
		hashBlob = precomputed.ContentHashBlob
	} else {
		h, err := CreateBlobFromContent(s.repo, []byte(contentHash))
		if err != nil {
			return err
		}
		hashBlob = h
	}
	entries[sessionPath+paths.V2RawTranscriptHashFileName] = object.TreeEntry{
		Name: sessionPath + paths.V2RawTranscriptHashFileName,
		Mode: filemode.Regular,
		Hash: hashBlob,
	}
	return nil
}

// validateWriteOpts validates identifiers in WriteCommittedOptions.
func validateWriteOpts(opts WriteCommittedOptions) error {
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
	return nil
}

// UpdateSummary persists an AI-generated summary into the latest session's
// metadata on the v2 /main ref. Mirrors GitStore.UpdateSummary for v1.
func (s *V2GitStore) UpdateSummary(ctx context.Context, checkpointID id.CheckpointID, summary *Summary) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	parentHash, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return ErrCheckpointNotFound
	}

	basePath := checkpointID.Path() + "/"
	checkpointPath := checkpointID.Path()
	entries, err := s.gs.flattenCheckpointEntries(rootTreeHash, checkpointPath)
	if err != nil {
		return err
	}

	rootMetadataPath := basePath + paths.MetadataFileName
	entry, exists := entries[rootMetadataPath]
	if !exists {
		return ErrCheckpointNotFound
	}

	cpSummary, err := readJSONFromBlob[CheckpointSummary](s.repo, entry.Hash)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint summary: %w", err)
	}
	if len(cpSummary.Sessions) == 0 {
		return ErrCheckpointNotFound
	}

	latestIndex := len(cpSummary.Sessions) - 1
	sessionMetadataPath := fmt.Sprintf("%s%d/%s", basePath, latestIndex, paths.MetadataFileName)
	sessionEntry, exists := entries[sessionMetadataPath]
	if !exists {
		return fmt.Errorf("session metadata not found at index %d", latestIndex)
	}

	metadata, err := readJSONFromBlob[CommittedMetadata](s.repo, sessionEntry.Hash)
	if err != nil {
		return fmt.Errorf("failed to read session metadata: %w", err)
	}
	metadata.Summary = redactSummary(summary)

	metadataJSON, err := jsonutil.MarshalIndentWithNewline(metadata, "", "  ")
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

	newTreeHash, err := s.gs.spliceCheckpointSubtree(ctx, rootTreeHash, checkpointID, basePath, entries)
	if err != nil {
		return err
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Update summary for checkpoint %s (session: %s)", checkpointID, metadata.SessionID)
	return s.updateRef(ctx, refName, newTreeHash, parentHash, commitMsg, authorName, authorEmail)
}

// CleanupV1TranscriptFiles removes legacy v1-named transcript files (full.jsonl,
// full.jsonl.*, content_hash.txt) from /full/current for a given checkpoint.
// Older CLI versions wrote these before the rename to raw_transcript.
// Returns nil if /full/current doesn't exist or no v1 files were found.
func (s *V2GitStore) CleanupV1TranscriptFiles(ctx context.Context, checkpointID id.CheckpointID, sessionCount int) error {
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil // /full/current doesn't exist yet — nothing to clean
		}
		return err
	}

	checkpointPath := checkpointID.Path()
	basePath := checkpointPath + "/"

	entries, err := s.gs.flattenCheckpointEntries(rootTreeHash, checkpointPath)
	if err != nil {
		return err
	}

	changed := false
	for sessionIdx := range sessionCount {
		sessionPath := fmt.Sprintf("%s%d/", basePath, sessionIdx)
		v1TranscriptPath := sessionPath + paths.TranscriptFileName
		v1HashPath := sessionPath + paths.ContentHashFileName

		for key := range entries {
			switch {
			case key == v1TranscriptPath,
				strings.HasPrefix(key, v1TranscriptPath+"."),
				key == v1HashPath:
				delete(entries, key)
				changed = true
			}
		}
	}

	if !changed {
		return nil
	}

	newTreeHash, err := s.gs.spliceCheckpointSubtree(ctx, rootTreeHash, checkpointID, basePath, entries)
	if err != nil {
		return fmt.Errorf("tree surgery failed: %w", err)
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	return s.updateRef(ctx, refName, newTreeHash, parentHash,
		fmt.Sprintf("Clean up v1 transcript files for %s\n", checkpointID),
		authorName, authorEmail)
}
