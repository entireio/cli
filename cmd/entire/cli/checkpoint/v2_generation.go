package checkpoint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage"
)

// DefaultMaxCheckpointsPerGeneration is the rotation threshold.
// When a generation reaches this many checkpoints, it is archived
// and a fresh /full/current is created.
const DefaultMaxCheckpointsPerGeneration = 100

// GenerationMetadata tracks the state of a /full/* generation.
// Written to the tree root as generation.json at archive time only — not during
// normal writes to /full/current. This keeps /full/current free of root-level
// files, ensuring conflict-free tree merges during push recovery.
//
// The generation's sequence number is derived from the ref name, not stored here.
// Checkpoint membership is determined by walking the tree (shard directories).
type GenerationMetadata struct {
	// OldestCheckpointAt is the creation time of the earliest checkpoint.
	OldestCheckpointAt time.Time `json:"oldest_checkpoint_at"`

	// NewestCheckpointAt is the creation time of the most recent checkpoint.
	NewestCheckpointAt time.Time `json:"newest_checkpoint_at"`
}

// ReadGeneration reads generation.json from the given tree hash.
// Returns a zero-value GenerationMetadata if the file doesn't exist (new/empty generation).
func (s *V2GitStore) ReadGeneration(treeHash plumbing.Hash) (GenerationMetadata, error) {
	if treeHash == plumbing.ZeroHash {
		return GenerationMetadata{}, nil
	}

	tree, err := s.repo.TreeObject(treeHash)
	if err != nil {
		return GenerationMetadata{}, fmt.Errorf("failed to read tree: %w", err)
	}

	file, err := tree.File(paths.GenerationFileName)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrEntryNotFound) {
			return GenerationMetadata{}, nil
		}
		return GenerationMetadata{}, fmt.Errorf("failed to find %s in tree: %w", paths.GenerationFileName, err)
	}

	content, err := file.Contents()
	if err != nil {
		return GenerationMetadata{}, fmt.Errorf("failed to read %s: %w", paths.GenerationFileName, err)
	}

	var gen GenerationMetadata
	if err := json.Unmarshal([]byte(content), &gen); err != nil {
		return GenerationMetadata{}, fmt.Errorf("failed to parse %s: %w", paths.GenerationFileName, err)
	}

	return gen, nil
}

// ReadGenerationFromRef reads generation.json from the tree pointed to by the given ref.
func (s *V2GitStore) ReadGenerationFromRef(refName plumbing.ReferenceName) (GenerationMetadata, error) {
	_, treeHash, err := s.GetRefState(refName)
	if err != nil {
		return GenerationMetadata{}, fmt.Errorf("failed to get ref state: %w", err)
	}
	return s.ReadGeneration(treeHash)
}

// marshalGenerationBlob marshals gen as generation.json and stores it as a git blob.
// Returns a TreeEntry ready to be placed in a tree.
func (s *V2GitStore) marshalGenerationBlob(gen GenerationMetadata) (object.TreeEntry, error) {
	data, err := jsonutil.MarshalIndentWithNewline(gen, "", "  ")
	if err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to marshal %s: %w", paths.GenerationFileName, err)
	}

	blobHash, err := CreateBlobFromContent(s.repo, data)
	if err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to create %s blob: %w", paths.GenerationFileName, err)
	}

	return object.TreeEntry{
		Name: paths.GenerationFileName,
		Mode: filemode.Regular,
		Hash: blobHash,
	}, nil
}

// writeGeneration marshals gen as generation.json and adds the blob entry to entries.
func (s *V2GitStore) writeGeneration(gen GenerationMetadata, entries map[string]object.TreeEntry) error {
	entry, err := s.marshalGenerationBlob(gen)
	if err != nil {
		return err
	}
	entries[paths.GenerationFileName] = entry
	return nil
}

// CountCheckpointsInTree counts checkpoint shard directories in a /full/* tree.
// The tree structure is <id[:2]>/<id[2:]>/ — we count second-level directories
// across all shard prefixes. Returns 0 for an empty tree.
func (s *V2GitStore) CountCheckpointsInTree(treeHash plumbing.Hash) (int, error) {
	if treeHash == plumbing.ZeroHash {
		return 0, nil
	}

	tree, err := s.repo.TreeObject(treeHash)
	if err != nil {
		return 0, fmt.Errorf("failed to read tree: %w", err)
	}

	count := 0
	if err := WalkCheckpointShards(s.repo, tree, func(_ id.CheckpointID, _ plumbing.Hash) error {
		count++
		return nil
	}); err != nil {
		return 0, err
	}

	return count, nil
}

// AddGenerationJSONToTree adds generation.json to an existing root tree, returning
// a new root tree hash. Preserves all existing entries (shard directories, etc.).
func (s *V2GitStore) AddGenerationJSONToTree(rootTreeHash plumbing.Hash, gen GenerationMetadata) (plumbing.Hash, error) {
	entry, err := s.marshalGenerationBlob(gen)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return UpdateSubtree(s.repo, rootTreeHash, nil, []object.TreeEntry{entry},
		UpdateSubtreeOptions{MergeMode: MergeKeepExisting})
}

// ComputeGenerationCheckpointTimestamps derives timestamps from the checkpoints
// present in a /full/* tree. It prefers created_at from v2 /main metadata and
// falls back to top-level transcript event timestamps for older or partial v2 data.
func (s *V2GitStore) ComputeGenerationCheckpointTimestamps(rootTreeHash plumbing.Hash) (GenerationMetadata, bool, error) {
	mainTree, mainTreeErr := s.v2MainTree()
	if mainTreeErr != nil {
		mainTree = nil
	}
	return s.ComputeGenerationTimestampsFromTrees(rootTreeHash, mainTree)
}

// ComputeGenerationTimestampsFromTrees walks every checkpoint in rootTreeHash
// and aggregates per-checkpoint timestamps. When mainTree is non-nil, /main
// metadata.json is consulted before falling back to the raw transcript inside
// the checkpoint's full-tree. Returns found=false when any checkpoint cannot
// produce a timestamp; callers decide their own fallback (e.g. read existing
// generation.json, recompute from in-memory data, or surface an error).
func (s *V2GitStore) ComputeGenerationTimestampsFromTrees(rootTreeHash plumbing.Hash, mainTree *object.Tree) (GenerationMetadata, bool, error) {
	if rootTreeHash == plumbing.ZeroHash {
		return GenerationMetadata{}, false, nil
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return GenerationMetadata{}, false, fmt.Errorf("failed to read generation tree: %w", err)
	}

	var gen GenerationMetadata
	found := false
	missingCheckpointTimestamp := false
	err = WalkCheckpointShards(s.repo, rootTree, func(cpID id.CheckpointID, cpTreeHash plumbing.Hash) error {
		if mainTree != nil {
			if cpGen, ok := s.checkpointTimestampRangeFromMain(mainTree, cpID); ok {
				mergeGenerationRange(&gen, &found, cpGen)
				return nil
			}
		}

		cpTree, treeErr := s.repo.TreeObject(cpTreeHash)
		if treeErr != nil {
			missingCheckpointTimestamp = true
			return nil //nolint:nilerr // Skip unreadable checkpoint trees and fall back to generation.json.
		}
		if cpGen, ok := checkpointTimestampRangeFromFullTree(cpTree); ok {
			mergeGenerationRange(&gen, &found, cpGen)
			return nil
		}
		missingCheckpointTimestamp = true
		return nil
	})
	if err != nil {
		return GenerationMetadata{}, false, err
	}
	if missingCheckpointTimestamp {
		return GenerationMetadata{}, false, nil
	}

	return gen, found, nil
}

// computeGenerationTimestamps derives timestamps for a generation being archived.
// It uses checkpoint metadata/transcript timestamps rather than git commit times
// so migration and ref-repair commits don't reset retention age.
func (s *V2GitStore) computeGenerationTimestamps(rootTreeHash plumbing.Hash) GenerationMetadata {
	if gen, ok, err := s.ComputeGenerationCheckpointTimestamps(rootTreeHash); err == nil && ok {
		return gen
	}
	return s.computeGenerationTimestampsFromCommitHistory()
}

func (s *V2GitStore) computeGenerationTimestampsFromCommitHistory() GenerationMetadata {
	now := time.Now().UTC()
	fallback := GenerationMetadata{OldestCheckpointAt: now, NewestCheckpointAt: now}

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return fallback
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return fallback
	}

	newest := commit.Committer.When.UTC()

	// Walk parents to find the oldest commit in this generation
	iter := commit
	for len(iter.ParentHashes) > 0 {
		parent, parentErr := s.repo.CommitObject(iter.ParentHashes[0])
		if parentErr != nil {
			break
		}
		iter = parent
	}
	oldest := iter.Committer.When.UTC()

	return GenerationMetadata{
		OldestCheckpointAt: oldest,
		NewestCheckpointAt: newest,
	}
}

func (s *V2GitStore) v2MainTree() (*object.Tree, error) {
	ref, err := s.repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	if err != nil {
		return nil, fmt.Errorf("failed to read v2 main ref: %w", err)
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to read v2 main commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to read v2 main tree: %w", err)
	}
	return tree, nil
}

func (s *V2GitStore) checkpointTimestampRangeFromMain(mainTree *object.Tree, cpID id.CheckpointID) (GenerationMetadata, bool) {
	cpTree, err := mainTree.Tree(cpID.Path())
	if err != nil {
		return GenerationMetadata{}, false
	}

	var gen GenerationMetadata
	found := false
	for _, entry := range cpTree.Entries {
		if entry.Mode != filemode.Dir {
			continue
		}
		if _, err := strconv.Atoi(entry.Name); err != nil {
			continue
		}
		sessionTree, err := s.repo.TreeObject(entry.Hash)
		if err != nil {
			continue
		}
		metadataFile, err := sessionTree.File(paths.MetadataFileName)
		if err != nil {
			continue
		}
		metadataContent, err := metadataFile.Contents()
		if err != nil {
			continue
		}
		var metadata CommittedMetadata
		if err := json.Unmarshal([]byte(metadataContent), &metadata); err != nil || metadata.CreatedAt.IsZero() {
			continue
		}
		MergeGenerationTime(&gen, &found, metadata.CreatedAt.UTC())
	}
	return gen, found
}

func checkpointTimestampRangeFromFullTree(cpTree *object.Tree) (GenerationMetadata, bool) {
	var gen GenerationMetadata
	found := false
	for _, entry := range cpTree.Entries {
		if entry.Mode != filemode.Dir {
			continue
		}
		if _, err := strconv.Atoi(entry.Name); err != nil {
			continue
		}
		sessionTree, err := cpTree.Tree(entry.Name)
		if err != nil {
			continue
		}
		transcript, err := readTranscriptFromObjectTree(sessionTree, "")
		if err != nil || len(transcript) == 0 {
			continue
		}
		if transcriptGen, ok := timestampRangeFromTranscript(transcript); ok {
			mergeGenerationRange(&gen, &found, transcriptGen)
		}
	}
	return gen, found
}

// AggregateTranscriptTimestamps derives a generation timestamp envelope from
// transcripts already in memory, using the same first/last-event semantics
// as ComputeGenerationTimestampsFromTrees but skipping the blob reads.
func AggregateTranscriptTimestamps(transcripts [][]byte) (GenerationMetadata, bool) {
	var gen GenerationMetadata
	found := false
	for _, transcript := range transcripts {
		if len(transcript) == 0 {
			continue
		}
		if r, ok := timestampRangeFromTranscript(transcript); ok {
			mergeGenerationRange(&gen, &found, r)
		}
	}
	return gen, found
}

func timestampRangeFromTranscript(transcript []byte) (GenerationMetadata, bool) {
	reader := bufio.NewReader(bytes.NewReader(transcript))
	var gen GenerationMetadata
	found := false

	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var event struct {
				Timestamp string `json:"timestamp"`
			}
			if jsonErr := json.Unmarshal(trimmed, &event); jsonErr == nil && event.Timestamp != "" {
				if ts, parseErr := time.Parse(time.RFC3339Nano, event.Timestamp); parseErr == nil {
					MergeGenerationTime(&gen, &found, ts.UTC())
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
	}

	return gen, found
}

func mergeGenerationRange(dst *GenerationMetadata, found *bool, src GenerationMetadata) {
	MergeGenerationTime(dst, found, src.OldestCheckpointAt)
	MergeGenerationTime(dst, found, src.NewestCheckpointAt)
}

// MergeGenerationTime expands the generation timestamp envelope to include ts.
// The found flag is set the first time a non-zero timestamp is observed.
func MergeGenerationTime(gen *GenerationMetadata, found *bool, ts time.Time) {
	if ts.IsZero() {
		return
	}
	ts = ts.UTC()
	if !*found {
		gen.OldestCheckpointAt = ts
		gen.NewestCheckpointAt = ts
		*found = true
		return
	}
	if ts.Before(gen.OldestCheckpointAt) {
		gen.OldestCheckpointAt = ts
	}
	if ts.After(gen.NewestCheckpointAt) {
		gen.NewestCheckpointAt = ts
	}
}

// generationRefWidth is the zero-padded width of archived generation ref names.
const generationRefWidth = 13

// ArchivedGenerationRefName returns the full ref name for an archived generation number.
func ArchivedGenerationRefName(number int) plumbing.ReferenceName {
	return plumbing.ReferenceName(fmt.Sprintf("%s%0*d", paths.V2FullRefPrefix, generationRefWidth, number))
}

// GenerationRefPattern matches exactly 13 digits (the archived generation ref suffix format).
var GenerationRefPattern = regexp.MustCompile(`^\d{13}$`)

// listArchivedGenerations returns the names of all archived generation refs
// (everything under V2FullRefPrefix matching the expected numeric format), sorted ascending.
func (s *V2GitStore) ListArchivedGenerations() ([]string, error) {
	refs, err := s.repo.References()
	if err != nil {
		return nil, fmt.Errorf("failed to list references: %w", err)
	}

	var archived []string
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if !strings.HasPrefix(name, paths.V2FullRefPrefix) {
			return nil
		}
		suffix := strings.TrimPrefix(name, paths.V2FullRefPrefix)
		if suffix == "current" || !GenerationRefPattern.MatchString(suffix) {
			return nil
		}
		archived = append(archived, suffix)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate references: %w", err)
	}

	sort.Strings(archived)
	return archived, nil
}

// NextGenerationNumber returns the next sequential generation number for archiving.
// Scans existing archived refs and returns max+1. Returns 1 if no archives exist.
func (s *V2GitStore) NextGenerationNumber() (int, error) {
	archived, err := s.ListArchivedGenerations()
	if err != nil {
		return 0, err
	}

	var maxNum int64
	for _, name := range archived {
		n, parseErr := strconv.ParseInt(name, 10, 64)
		if parseErr != nil {
			continue // skip unparseable entries
		}
		if n > maxNum {
			maxNum = n
		}
	}
	return int(maxNum) + 1, nil
}

// RotateCurrentGenerationIfNeeded archives /full/current when it has reached
// maxCheckpoints. It returns the archived ref name when this call completes
// a rotation.
func (s *V2GitStore) RotateCurrentGenerationIfNeeded(ctx context.Context, maxCheckpoints int) (plumbing.ReferenceName, bool, error) {
	if maxCheckpoints <= 0 {
		maxCheckpoints = s.maxCheckpoints()
	}

	// This is a 2-phase operation:
	//
	//  1. Archive: determine the next generation number, create a new ref pointing
	//     to the current /full/current commit.
	//  2. Reset: create a fresh orphan commit with an empty tree, point
	//     /full/current at it.
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)

	// Guard against concurrent rotation: re-read /full/current and check if
	// it's still above the threshold. If not, another instance already rotated.
	_, currentTreeHash, err := s.GetRefState(refName)
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to read /full/current: %w", err)
	}
	checkpointCount, err := s.CountCheckpointsInTree(currentTreeHash)
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to count checkpoints: %w", err)
	}
	if checkpointCount < maxCheckpoints {
		return "", false, nil
	}

	currentRef, err := s.repo.Reference(refName, true)
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to read /full/current ref: %w", err)
	}

	archiveNumber, err := s.NextGenerationNumber()
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to determine next generation number: %w", err)
	}

	// Phase 1: Archive — create ref pointing to the current commit.
	// If the archive ref already exists, another instance already rotated — skip.
	archiveRefName := ArchivedGenerationRefName(archiveNumber)
	if _, refErr := s.repo.Reference(archiveRefName, true); refErr == nil {
		logging.Info(ctx, "rotation: archive ref already exists, skipping",
			slog.String("archive_ref", string(archiveRefName)),
		)
		return archiveRefName, false, nil
	}
	archiveRef := plumbing.NewHashReference(archiveRefName, currentRef.Hash())
	if err := s.repo.Storer.SetReference(archiveRef); err != nil {
		return "", false, fmt.Errorf("rotation: failed to create archived ref %s: %w", archiveRefName, err)
	}

	// Verify /full/current hasn't been advanced by another writer since we read it.
	// If it changed, abort — the archive ref is harmless (points to a valid commit)
	// and the next writer will trigger rotation again.
	postArchiveRef, err := s.repo.Reference(refName, true)
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to re-read /full/current: %w", err)
	}
	if postArchiveRef.Hash() != currentRef.Hash() {
		logging.Info(ctx, "rotation: /full/current changed during rotation, aborting reset")
		return archiveRefName, false, nil
	}

	// Write generation.json to the current tree before archiving.
	gen := s.computeGenerationTimestamps(currentTreeHash)
	archiveTreeHash, err := s.AddGenerationJSONToTree(currentTreeHash, gen)
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to add generation.json: %w", err)
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	archiveCommitHash, err := CreateCommit(ctx, s.repo, archiveTreeHash, currentRef.Hash(), "Archive generation", authorName, authorEmail)
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to create archive commit: %w", err)
	}

	// Update the archive ref to point to the commit with generation.json
	archiveRef = plumbing.NewHashReference(archiveRefName, archiveCommitHash)
	if err := s.repo.Storer.SetReference(archiveRef); err != nil {
		return "", false, fmt.Errorf("rotation: failed to update archived ref %s: %w", archiveRefName, err)
	}

	// Phase 2: Create fresh orphan /full/current (empty tree, no generation.json)
	emptyTreeHash, err := BuildTreeFromEntries(ctx, s.repo, make(map[string]object.TreeEntry))
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to build empty tree: %w", err)
	}

	orphanCommitHash, err := CreateCommit(ctx, s.repo, emptyTreeHash, plumbing.ZeroHash, "Start generation", authorName, authorEmail)
	if err != nil {
		return "", false, fmt.Errorf("rotation: failed to create orphan commit: %w", err)
	}

	reset, err := s.resetFullCurrentRefIfUnchanged(ctx, refName, postArchiveRef, orphanCommitHash)
	if err != nil {
		return "", false, err
	}
	if !reset {
		return archiveRefName, false, nil
	}

	logging.Info(ctx, "generation rotation complete",
		slog.Int("archived_generation", archiveNumber),
		slog.String("archive_ref", string(archiveRefName)),
	)

	return archiveRefName, true, nil
}

func (s *V2GitStore) resetFullCurrentRefIfUnchanged(ctx context.Context, refName plumbing.ReferenceName, expectedRef *plumbing.Reference, newHash plumbing.Hash) (bool, error) {
	newRef := plumbing.NewHashReference(refName, newHash)
	if err := s.repo.Storer.CheckAndSetReference(newRef, expectedRef); err != nil {
		if errors.Is(err, storage.ErrReferenceHasChanged) {
			logging.Info(ctx, "rotation: /full/current changed before reset, leaving current ref intact")
			return false, nil
		}
		return false, fmt.Errorf("rotation: failed to reset /full/current: %w", err)
	}
	return true, nil
}

func (s *V2GitStore) rotateGeneration(ctx context.Context) error {
	_, _, err := s.RotateCurrentGenerationIfNeeded(ctx, s.maxCheckpoints())
	return err
}
