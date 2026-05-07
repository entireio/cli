package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// ReadCommitted reads the checkpoint summary from the v2 /main ref.
// Returns nil, nil if the checkpoint doesn't exist (same contract as GitStore.ReadCommitted).
func (s *V2GitStore) ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	_, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Ref doesn't exist means no checkpoint
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Tree not readable
	}

	cpTree, err := rootTree.Tree(checkpointID.Path())
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Checkpoint subtree not found
	}

	cpFT := s.wrapWithFetcher(ctx, cpTree)
	metadataFile, err := cpFT.File(paths.MetadataFileName)
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

// ListCommitted lists all committed checkpoints from the v2 /main ref.
// Scans sharded paths: <id[:2]>/<id[2:]>/ directories containing metadata.json.
func (s *V2GitStore) ListCommitted(ctx context.Context) ([]CommittedInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	_, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return []CommittedInfo{}, nil //nolint:nilerr // No /main ref means empty list
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return []CommittedInfo{}, nil //nolint:nilerr // Unreadable tree means no listable entries
	}

	var checkpoints []CommittedInfo

	_ = WalkCheckpointShards(s.repo, rootTree, func(checkpointID id.CheckpointID, cpTreeHash plumbing.Hash) error { //nolint:errcheck // callback never returns errors
		checkpointTree, cpTreeErr := s.repo.TreeObject(cpTreeHash)
		if cpTreeErr != nil {
			logging.Debug(ctx, "v2 ListCommitted: skipping unreadable checkpoint tree",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("error", cpTreeErr.Error()))
			return nil
		}

		info := CommittedInfo{CheckpointID: checkpointID}

		if metadataFile, fileErr := checkpointTree.File(paths.MetadataFileName); fileErr == nil {
			if content, contentErr := metadataFile.Contents(); contentErr == nil {
				var summary CheckpointSummary
				if unmarshalErr := json.Unmarshal([]byte(content), &summary); unmarshalErr != nil {
					logging.Debug(ctx, "v2 ListCommitted: skipping malformed metadata",
						slog.String("checkpoint_id", checkpointID.String()),
						slog.String("error", unmarshalErr.Error()))
				} else {
					info.CheckpointsCount = summary.CheckpointsCount
					info.FilesTouched = summary.FilesTouched
					info.SessionCount = len(summary.Sessions)

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
		return nil
	})

	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})

	return checkpoints, nil
}

// ReadSessionCompactTranscript reads transcript.jsonl for a session from the v2
// /main ref. Returns ErrNoTranscript when compact transcript is missing.
func (s *V2GitStore) ReadSessionCompactTranscript(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	_, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	cpTree, err := rootTree.Tree(checkpointID.Path())
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	sessionDir := strconv.Itoa(sessionIndex)
	sessionTree, err := cpTree.Tree(sessionDir)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	sessionFT := s.wrapWithFetcher(ctx, sessionTree)
	compactFile, err := sessionFT.File(paths.CompactTranscriptFileName)
	if err != nil {
		return nil, ErrNoTranscript
	}

	content, err := compactFile.Contents()
	if err != nil {
		return nil, ErrNoTranscript
	}
	if content == "" {
		return nil, ErrNoTranscript
	}

	return []byte(content), nil
}

// ReadSessionMetadata reads only the session's metadata.json from the v2
// /main ref — cheaper than ReadSessionMetadataAndPrompts when the caller
// only needs metadata fields (e.g. SessionID, Kind, Agent). Lazy-fetches
// the metadata blob via wrapWithFetcher when missing locally. Returns
// ErrCheckpointNotFound if the checkpoint or session doesn't exist on /main.
func (s *V2GitStore) ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*CommittedMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	_, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	cpTree, err := rootTree.Tree(checkpointID.Path())
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	sessionTree, err := cpTree.Tree(strconv.Itoa(sessionIndex))
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	sessionFT := s.wrapWithFetcher(ctx, sessionTree)
	metadataFile, err := sessionFT.File(paths.MetadataFileName)
	if err != nil {
		return nil, fmt.Errorf("read session metadata file: %w", err)
	}
	content, err := metadataFile.Contents()
	if err != nil {
		return nil, fmt.Errorf("read session metadata contents: %w", err)
	}

	var meta CommittedMetadata
	if err := json.Unmarshal([]byte(content), &meta); err != nil {
		return nil, fmt.Errorf("parse session metadata: %w", err)
	}
	return &meta, nil
}

// ReadSessionMetadataAndPrompts reads a session's metadata and prompts from the
// v2 /main ref without requiring the raw transcript from /full/* refs.
// Used by explain when the raw transcript is unavailable but compact transcript
// (transcript.jsonl) on /main can substitute for display.
// Returns ErrCheckpointNotFound if the checkpoint or session doesn't exist on /main.
func (s *V2GitStore) ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	sessionTree, err := s.sessionTreeFromMain(ctx, checkpointID, sessionIndex)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
		}
		return nil, ErrCheckpointNotFound
	}

	result := &SessionContent{}
	sessionFT := s.wrapWithFetcher(ctx, sessionTree)

	if metadataFile, fileErr := sessionFT.File(paths.MetadataFileName); fileErr == nil {
		if content, contentErr := metadataFile.Contents(); contentErr == nil {
			if jsonErr := json.Unmarshal([]byte(content), &result.Metadata); jsonErr != nil {
				return nil, fmt.Errorf("failed to parse session metadata: %w", jsonErr)
			}
		}
	}

	if file, fileErr := sessionFT.File(paths.PromptFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			result.Prompts = content
		}
	}

	// Read compact transcript from the same session tree (avoids a second tree walk).
	if compactFile, fileErr := sessionFT.File(paths.CompactTranscriptFileName); fileErr == nil {
		if content, contentErr := compactFile.Contents(); contentErr == nil && content != "" {
			result.Transcript = []byte(content)
		}
	}

	return result, nil
}

// ReadSessionContent reads a session's metadata and prompts from the v2 /main ref,
// and the raw transcript (raw_transcript) from /full/* refs (current + archived generations).
// This is the v2 equivalent of GitStore.ReadSessionContent — it reads the raw agent
// transcript, not the compact transcript.jsonl. Used by resume and RestoreLogsOnly.
// Returns ErrNoTranscript if the session exists but no raw transcript is available.
// Returns ErrCheckpointNotFound if the checkpoint or session doesn't exist on /main.
func (s *V2GitStore) ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	sessionTree, err := s.sessionTreeFromMain(ctx, checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}

	result := &SessionContent{}
	sessionFT := s.wrapWithFetcher(ctx, sessionTree)

	if metadataFile, fileErr := sessionFT.File(paths.MetadataFileName); fileErr == nil {
		if content, contentErr := metadataFile.Contents(); contentErr == nil {
			if jsonErr := json.Unmarshal([]byte(content), &result.Metadata); jsonErr != nil {
				return nil, fmt.Errorf("failed to parse session metadata: %w", jsonErr)
			}
		}
	}

	if file, fileErr := sessionFT.File(paths.PromptFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			result.Prompts = content
		}
	}

	transcript, transcriptErr := s.readTranscriptFromFullRefs(ctx, checkpointID, sessionIndex, result.Metadata.Agent)
	if transcriptErr != nil {
		return nil, fmt.Errorf("failed to read transcript from /full/* refs: %w", transcriptErr)
	}
	if len(transcript) == 0 {
		return nil, ErrNoTranscript
	}
	result.Transcript = transcript

	return result, nil
}

func (s *V2GitStore) sessionTreeFromMain(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*object.Tree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	_, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	cpTree, err := rootTree.Tree(checkpointID.Path())
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	sessionDir := strconv.Itoa(sessionIndex)
	sessionTree, err := cpTree.Tree(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("session %d not found: %w", sessionIndex, err)
	}

	return sessionTree, nil
}

// readTranscriptFromFullRefs reads the raw transcript for a checkpoint session
// by searching /full/current first, then archived generations in reverse order.
// If not found locally, attempts to discover and fetch remote /full/* refs.
func (s *V2GitStore) readTranscriptFromFullRefs(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, agentType types.AgentType) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	sessionPath := fmt.Sprintf("%s/%d", checkpointID.Path(), sessionIndex)

	// Search locally first
	transcript, err := s.readTranscriptFromRef(plumbing.ReferenceName(paths.V2FullCurrentRefName), sessionPath, agentType)
	if err == nil && len(transcript) > 0 {
		return transcript, nil
	}

	archived, err := s.ListArchivedGenerations()
	if err != nil {
		return nil, err
	}
	for i := len(archived) - 1; i >= 0; i-- {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + archived[i])
		transcript, err := s.readTranscriptFromRef(refName, sessionPath, agentType)
		if err == nil && len(transcript) > 0 {
			return transcript, nil
		}
	}

	// Not found locally — try fetching remote /full/* refs
	if fetchErr := s.fetchRemoteFullRefs(ctx); fetchErr != nil {
		logging.Debug(ctx, "failed to fetch remote /full/* refs",
			slog.String("error", fetchErr.Error()),
		)
		return nil, nil
	}

	// Search newly fetched refs only
	newArchived, err := s.ListArchivedGenerations()
	if err != nil {
		return nil, nil //nolint:nilerr // Best-effort: fetch-on-demand failure shouldn't block resume
	}
	existingSet := make(map[string]bool, len(archived))
	for _, a := range archived {
		existingSet[a] = true
	}
	for i := len(newArchived) - 1; i >= 0; i-- {
		if existingSet[newArchived[i]] {
			continue
		}
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + newArchived[i])
		transcript, err := s.readTranscriptFromRef(refName, sessionPath, agentType)
		if err == nil && len(transcript) > 0 {
			return transcript, nil
		}
	}

	// Also retry /full/current in case it was updated by the fetch
	transcript, err = s.readTranscriptFromRef(plumbing.ReferenceName(paths.V2FullCurrentRefName), sessionPath, agentType)
	if err == nil && len(transcript) > 0 {
		return transcript, nil
	}

	return nil, nil
}

// fetchRemoteFullRefs discovers and fetches /full/* refs from the configured
// FetchRemote that aren't local.
func (s *V2GitStore) fetchRemoteFullRefs(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	lsCmd := exec.CommandContext(ctx, "git", "ls-remote", s.FetchRemote, paths.V2FullRefPrefix+"*")
	output, err := lsCmd.Output()
	if err != nil {
		return fmt.Errorf("ls-remote failed: %w", err)
	}

	var refSpecs []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		remoteRefName := parts[1]

		// Skip refs that already exist locally
		if _, refErr := s.repo.Reference(plumbing.ReferenceName(remoteRefName), true); refErr == nil {
			continue
		}

		refSpecs = append(refSpecs, fmt.Sprintf("+%s:%s", remoteRefName, remoteRefName))
	}

	if len(refSpecs) == 0 {
		return nil
	}

	args := append([]string{"fetch", "--no-tags", s.FetchRemote}, refSpecs...)
	fetchCmd := exec.CommandContext(ctx, "git", args...)
	if fetchOutput, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
		return fmt.Errorf("fetch failed: %s", fetchOutput)
	}

	return nil
}

// readTranscriptFromRef reads the raw transcript from a specific /full/* ref.
// Follows the same chunking convention as readTranscriptFromTree in committed.go:
// chunk 0 is the base file (raw_transcript), chunks 1+ are raw_transcript.001, .002, etc.
// When chunk files exist, all chunks (including chunk 0) are reassembled using
// agent-aware reassembly via agent.ReassembleTranscript.
func (s *V2GitStore) readTranscriptFromRef(refName plumbing.ReferenceName, sessionPath string, agentType types.AgentType) ([]byte, error) {
	_, rootTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return nil, err
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, fmt.Errorf("failed to read tree: %w", err)
	}

	sessionTree, err := rootTree.Tree(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("session path %s not found: %w", sessionPath, err)
	}

	return readTranscriptFromObjectTree(sessionTree, agentType)
}

// readTranscriptFromObjectTree reads and reassembles a transcript from a git tree object.
// Handles both chunked and non-chunked transcripts. Uses agent-aware reassembly
// when agentType is known, falling back to JSONL reassembly otherwise.
func readTranscriptFromObjectTree(tree *object.Tree, agentType types.AgentType) ([]byte, error) {
	var chunkFiles []string
	var hasBaseFile bool

	for _, entry := range tree.Entries {
		if entry.Name == paths.V2RawTranscriptFileName {
			hasBaseFile = true
		}
		if strings.HasPrefix(entry.Name, paths.V2RawTranscriptFileName+".") {
			idx := agent.ParseChunkIndex(entry.Name, paths.V2RawTranscriptFileName)
			if idx > 0 {
				chunkFiles = append(chunkFiles, entry.Name)
			}
		}
	}

	// If chunk files exist, reassemble all chunks (base file is chunk 0)
	if len(chunkFiles) > 0 {
		chunkFiles = agent.SortChunkFiles(chunkFiles, paths.V2RawTranscriptFileName)
		if hasBaseFile {
			chunkFiles = append([]string{paths.V2RawTranscriptFileName}, chunkFiles...)
		}

		var chunks [][]byte
		for _, chunkFile := range chunkFiles {
			file, fileErr := tree.File(chunkFile)
			if fileErr != nil {
				continue
			}
			content, contentErr := file.Contents()
			if contentErr != nil {
				continue
			}
			chunks = append(chunks, []byte(content))
		}

		if len(chunks) > 0 {
			result, reassembleErr := agent.ReassembleTranscript(chunks, agentType)
			if reassembleErr != nil {
				return nil, fmt.Errorf("failed to reassemble transcript: %w", reassembleErr)
			}
			return result, nil
		}
	}

	// No chunk files — read base file directly (non-chunked transcript)
	if hasBaseFile {
		file, err := tree.File(paths.V2RawTranscriptFileName)
		if err == nil {
			content, contentErr := file.Contents()
			if contentErr == nil {
				return []byte(content), nil
			}
		}
	}

	return nil, nil
}

// ReadSessionContentByID finds the session with the given sessionID in a checkpoint
// and returns its content. Mirrors GitStore.ReadSessionContentByID for v2 refs.
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist; returns a
// non-wrapped error (containing the session ID and checkpoint ID for context)
// if no session in the checkpoint matches sessionID.
func (s *V2GitStore) ReadSessionContentByID(ctx context.Context, checkpointID id.CheckpointID, sessionID string) (*SessionContent, error) {
	summary, err := s.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}

	for i := range summary.Sessions {
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

// GetSessionLog reads the latest session's raw transcript and session ID from v2 refs.
// Convenience wrapper matching the GitStore.GetSessionLog signature.
func (s *V2GitStore) GetSessionLog(ctx context.Context, cpID id.CheckpointID) ([]byte, string, error) {
	summary, err := s.ReadCommitted(ctx, cpID)
	if err != nil {
		return nil, "", err
	}
	if summary == nil {
		return nil, "", ErrCheckpointNotFound
	}
	if len(summary.Sessions) == 0 {
		return nil, "", ErrCheckpointNotFound
	}

	latestIndex := len(summary.Sessions) - 1
	content, err := s.ReadSessionContent(ctx, cpID, latestIndex)
	if err != nil {
		return nil, "", err
	}
	return content.Transcript, content.Metadata.SessionID, nil
}
