package trail

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const (
	metadataFile    = "metadata.json"
	discussionFile  = "discussion.json"
	checkpointsFile = "checkpoints.json"
)

// ErrTrailNotFound is returned when a trail cannot be found.
var ErrTrailNotFound = errors.New("trail not found")

// Store provides CRUD operations for trail metadata on the entire/trails/v1 branch.
type Store struct {
	repo *git.Repository
}

// NewStore creates a new trail store backed by the given git repository.
func NewStore(repo *git.Repository) *Store {
	return &Store{repo: repo}
}

// EnsureBranch creates the entire/trails/v1 orphan branch if it doesn't exist.
func (s *Store) EnsureBranch() error {
	refName := plumbing.NewBranchReferenceName(paths.TrailsBranchName)
	_, err := s.repo.Reference(refName, true)
	if err == nil {
		return nil // Branch already exists
	}

	// Create orphan branch with empty tree
	emptyTreeHash, err := checkpoint.BuildTreeFromEntries(s.repo, make(map[string]object.TreeEntry))
	if err != nil {
		return fmt.Errorf("failed to build empty tree: %w", err)
	}

	authorName, authorEmail := checkpoint.GetGitAuthorFromRepo(s.repo)
	commitHash, err := checkpoint.CreateCommit(s.repo, emptyTreeHash, plumbing.ZeroHash, "Initialize trails branch", authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("failed to create initial commit: %w", err)
	}

	newRef := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to set branch reference: %w", err)
	}
	return nil
}

// Write writes trail metadata, discussion, and checkpoints to the entire/trails/v1 branch.
// If checkpoints is nil, an empty checkpoints list is written.
func (s *Store) Write(metadata *Metadata, discussion *Discussion, checkpoints *Checkpoints) error {
	if metadata.TrailID.IsEmpty() {
		return errors.New("trail ID is required")
	}

	if err := s.EnsureBranch(); err != nil {
		return fmt.Errorf("failed to ensure trails branch: %w", err)
	}

	commitHash, rootTreeHash, err := s.getBranchRef()
	if err != nil {
		return fmt.Errorf("failed to get branch ref: %w", err)
	}

	// Build blob entries for the trail's 3 files
	trailEntries, err := s.buildTrailEntries(metadata, discussion, checkpoints)
	if err != nil {
		return err
	}

	// Splice into tree at [shard, suffix] — preserves sibling trails automatically
	shard, suffix := metadata.TrailID.ShardParts()
	newTreeHash, err := checkpoint.UpdateSubtree(
		s.repo, rootTreeHash,
		[]string{shard, suffix},
		trailEntries,
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.ReplaceAll},
	)
	if err != nil {
		return fmt.Errorf("failed to update subtree: %w", err)
	}

	commitMsg := fmt.Sprintf("Trail: %s (%s)", metadata.Title, metadata.TrailID)
	return s.commitAndUpdateRef(newTreeHash, commitHash, commitMsg)
}

// buildTrailEntries creates blob objects for a trail's 3 files and returns them as tree entries.
func (s *Store) buildTrailEntries(metadata *Metadata, discussion *Discussion, checkpoints *Checkpoints) ([]object.TreeEntry, error) {
	if discussion == nil {
		discussion = &Discussion{Comments: []Comment{}}
	}
	if checkpoints == nil {
		checkpoints = &Checkpoints{Checkpoints: []CheckpointRef{}}
	}

	type fileSpec struct {
		name string
		data any
	}
	files := []fileSpec{
		{metadataFile, metadata},
		{discussionFile, discussion},
		{checkpointsFile, checkpoints},
	}

	entries := make([]object.TreeEntry, 0, len(files))
	for _, f := range files {
		jsonBytes, err := json.MarshalIndent(f.data, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal %s: %w", f.name, err)
		}
		blobHash, err := checkpoint.CreateBlobFromContent(s.repo, jsonBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to create %s blob: %w", f.name, err)
		}
		entries = append(entries, object.TreeEntry{
			Name: f.name,
			Mode: filemode.Regular,
			Hash: blobHash,
		})
	}

	return entries, nil
}

// Read reads a trail by its ID from the entire/trails/v1 branch.
func (s *Store) Read(trailID ID) (*Metadata, *Discussion, *Checkpoints, error) {
	if err := ValidateID(string(trailID)); err != nil {
		return nil, nil, nil, err
	}

	tree, err := s.getBranchTree()
	if err != nil {
		return nil, nil, nil, err
	}

	basePath := trailID.Path() + "/"

	// Read metadata
	metadataEntry, err := tree.FindEntry(basePath + metadataFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("trail %s not found: %w", trailID, err)
	}
	metadataBlob, err := s.repo.BlobObject(metadataEntry.Hash)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read metadata blob: %w", err)
	}
	metadataReader, err := metadataBlob.Reader()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open metadata reader: %w", err)
	}
	defer metadataReader.Close()

	var metadata Metadata
	if err := json.NewDecoder(metadataReader).Decode(&metadata); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to decode metadata: %w", err)
	}

	// Read discussion (optional, may not exist yet)
	var discussion Discussion
	discussionEntry, err := tree.FindEntry(basePath + discussionFile)
	if err == nil {
		discussionBlob, blobErr := s.repo.BlobObject(discussionEntry.Hash)
		if blobErr == nil {
			discussionReader, readerErr := discussionBlob.Reader()
			if readerErr == nil {
				//nolint:errcheck,gosec // best-effort decode of optional discussion
				json.NewDecoder(discussionReader).Decode(&discussion)
				_ = discussionReader.Close()
			}
		}
	}

	// Read checkpoints (optional, may not exist yet)
	var checkpoints Checkpoints
	checkpointsEntry, err := tree.FindEntry(basePath + checkpointsFile)
	if err == nil {
		checkpointsBlob, blobErr := s.repo.BlobObject(checkpointsEntry.Hash)
		if blobErr == nil {
			checkpointsReader, readerErr := checkpointsBlob.Reader()
			if readerErr == nil {
				//nolint:errcheck,gosec // best-effort decode of optional checkpoints
				json.NewDecoder(checkpointsReader).Decode(&checkpoints)
				_ = checkpointsReader.Close()
			}
		}
	}

	return &metadata, &discussion, &checkpoints, nil
}

// FindByBranch finds a trail for the given branch name.
// Returns (nil, nil) if no trail exists for the branch.
func (s *Store) FindByBranch(branchName string) (*Metadata, error) {
	trails, err := s.List()
	if err != nil {
		return nil, err
	}

	for _, t := range trails {
		if t.Branch == branchName {
			return t, nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means "not found" — callers check both
}

// List returns all trail metadata from the entire/trails/v1 branch.
func (s *Store) List() ([]*Metadata, error) {
	tree, err := s.getBranchTree()
	if err != nil {
		// Branch doesn't exist yet — no trails
		return nil, nil //nolint:nilerr // Expected when no trails exist yet
	}

	var trails []*Metadata
	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(s.repo, tree, "", entries); err != nil {
		return nil, fmt.Errorf("failed to flatten tree: %w", err)
	}

	// Find all metadata.json files
	for path, entry := range entries {
		if !strings.HasSuffix(path, "/"+metadataFile) {
			continue
		}

		blob, err := s.repo.BlobObject(entry.Hash)
		if err != nil {
			continue
		}
		reader, err := blob.Reader()
		if err != nil {
			continue
		}

		var metadata Metadata
		decodeErr := json.NewDecoder(reader).Decode(&metadata)
		_ = reader.Close()
		if decodeErr != nil {
			continue
		}

		trails = append(trails, &metadata)
	}

	return trails, nil
}

// Update updates an existing trail's metadata. It reads the current metadata,
// applies the provided update function, and writes it back.
func (s *Store) Update(trailID ID, updateFn func(*Metadata)) error {
	// ValidateID is called by Read, no need to duplicate here
	metadata, discussion, checkpoints, err := s.Read(trailID)
	if err != nil {
		return fmt.Errorf("failed to read trail for update: %w", err)
	}

	updateFn(metadata)
	metadata.UpdatedAt = time.Now()

	return s.Write(metadata, discussion, checkpoints)
}

// AddCheckpoint prepends a checkpoint reference to a trail's checkpoints list (newest first).
// Only reads and writes the checkpoints.json file — metadata and discussion are untouched.
func (s *Store) AddCheckpoint(trailID ID, ref CheckpointRef) error {
	if err := ValidateID(string(trailID)); err != nil {
		return err
	}

	if err := s.EnsureBranch(); err != nil {
		return fmt.Errorf("failed to ensure trails branch: %w", err)
	}

	commitHash, rootTreeHash, err := s.getBranchRef()
	if err != nil {
		return fmt.Errorf("failed to get branch ref: %w", err)
	}

	// Navigate to the trail's subtree and read only checkpoints.json
	shard, suffix := trailID.ShardParts()
	trailTree, err := s.navigateToTrailTree(rootTreeHash, shard, suffix)
	if err != nil {
		return fmt.Errorf("failed to read checkpoints for trail %s: %w", trailID, err)
	}

	checkpoints, err := s.readCheckpointsFromTrailTree(trailTree)
	if err != nil {
		return fmt.Errorf("failed to read checkpoints for trail %s: %w", trailID, err)
	}

	// Prepend new ref (newest first)
	checkpoints.Checkpoints = append(checkpoints.Checkpoints, CheckpointRef{})
	copy(checkpoints.Checkpoints[1:], checkpoints.Checkpoints[:len(checkpoints.Checkpoints)-1])
	checkpoints.Checkpoints[0] = ref

	// Create new blob and splice back — MergeKeepExisting preserves metadata.json and discussion.json
	checkpointsJSON, err := json.MarshalIndent(checkpoints, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoints: %w", err)
	}
	blobHash, err := checkpoint.CreateBlobFromContent(s.repo, checkpointsJSON)
	if err != nil {
		return fmt.Errorf("failed to create checkpoints blob: %w", err)
	}

	newTreeHash, err := checkpoint.UpdateSubtree(
		s.repo, rootTreeHash,
		[]string{shard, suffix},
		[]object.TreeEntry{{Name: checkpointsFile, Mode: filemode.Regular, Hash: blobHash}},
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	if err != nil {
		return fmt.Errorf("failed to update subtree: %w", err)
	}

	commitMsg := fmt.Sprintf("Add checkpoint to trail: %s", trailID)
	return s.commitAndUpdateRef(newTreeHash, commitHash, commitMsg)
}

// Delete removes a trail from the entire/trails/v1 branch.
func (s *Store) Delete(trailID ID) error {
	if err := ValidateID(string(trailID)); err != nil {
		return err
	}

	if err := s.EnsureBranch(); err != nil {
		return fmt.Errorf("failed to ensure trails branch: %w", err)
	}

	commitHash, rootTreeHash, err := s.getBranchRef()
	if err != nil {
		return fmt.Errorf("failed to get branch ref: %w", err)
	}

	// Verify the trail exists by navigating the tree at O(depth)
	shard, suffix := trailID.ShardParts()
	if _, err := s.navigateToTrailTree(rootTreeHash, shard, suffix); err != nil {
		return err
	}

	// Delete the trail's subtree by removing it from the shard directory
	newTreeHash, err := checkpoint.UpdateSubtree(
		s.repo, rootTreeHash,
		[]string{shard},
		nil,
		checkpoint.UpdateSubtreeOptions{
			MergeMode:   checkpoint.MergeKeepExisting,
			DeleteNames: []string{suffix},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update subtree: %w", err)
	}

	commitMsg := fmt.Sprintf("Delete trail: %s", trailID)
	return s.commitAndUpdateRef(newTreeHash, commitHash, commitMsg)
}

// navigateToTrailTree walks rootTree → shard → suffix and returns the trail's subtree.
func (s *Store) navigateToTrailTree(rootTreeHash plumbing.Hash, shard, suffix string) (*object.Tree, error) {
	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, fmt.Errorf("trail %s/%s not found: %w", shard, suffix, err)
	}

	shardEntry, err := rootTree.FindEntry(shard)
	if err != nil {
		return nil, fmt.Errorf("trail %s/%s not found: %w", shard, suffix, err)
	}

	shardTree, err := s.repo.TreeObject(shardEntry.Hash)
	if err != nil {
		return nil, fmt.Errorf("trail %s/%s not found: %w", shard, suffix, err)
	}

	trailEntry, err := shardTree.FindEntry(suffix)
	if err != nil {
		return nil, fmt.Errorf("trail %s/%s not found: %w", shard, suffix, err)
	}

	trailTree, err := s.repo.TreeObject(trailEntry.Hash)
	if err != nil {
		return nil, fmt.Errorf("trail %s/%s not found: %w", shard, suffix, err)
	}

	return trailTree, nil
}

// readCheckpointsFromTrailTree reads checkpoints.json from a trail's subtree.
// Returns empty checkpoints if the file doesn't exist yet.
func (s *Store) readCheckpointsFromTrailTree(trailTree *object.Tree) (*Checkpoints, error) {
	cpEntry, err := trailTree.FindEntry(checkpointsFile)
	if err != nil {
		// No checkpoints file yet — return empty
		return &Checkpoints{Checkpoints: []CheckpointRef{}}, nil
	}

	blob, err := s.repo.BlobObject(cpEntry.Hash)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoints blob: %w", err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to open checkpoints reader: %w", err)
	}
	defer reader.Close()

	var checkpoints Checkpoints
	if err := json.NewDecoder(reader).Decode(&checkpoints); err != nil {
		return nil, fmt.Errorf("failed to decode checkpoints: %w", err)
	}

	return &checkpoints, nil
}

// commitAndUpdateRef creates a commit and updates the trails branch reference.
func (s *Store) commitAndUpdateRef(treeHash, parentHash plumbing.Hash, message string) error {
	authorName, authorEmail := checkpoint.GetGitAuthorFromRepo(s.repo)
	commitHash, err := checkpoint.CreateCommit(s.repo, treeHash, parentHash, message, authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}

	newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.TrailsBranchName), commitHash)
	if err := s.repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to update branch reference: %w", err)
	}
	return nil
}

// getBranchRef returns the commit hash and root tree hash for the entire/trails/v1 branch HEAD
// without flattening the tree. Falls back to remote tracking branch if local is missing.
func (s *Store) getBranchRef() (commitHash, rootTreeHash plumbing.Hash, err error) {
	refName := plumbing.NewBranchReferenceName(paths.TrailsBranchName)
	ref, refErr := s.repo.Reference(refName, true)
	if refErr != nil {
		// Try remote tracking branch
		remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.TrailsBranchName)
		ref, refErr = s.repo.Reference(remoteRefName, true)
		if refErr != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("trails branch not found: %w", refErr)
		}
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get commit: %w", err)
	}

	return ref.Hash(), commit.TreeHash, nil
}

// getBranchTree returns the tree for the entire/trails/v1 branch HEAD.
func (s *Store) getBranchTree() (*object.Tree, error) {
	_, rootTreeHash, err := s.getBranchRef()
	if err != nil {
		return nil, err
	}

	tree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	return tree, nil
}
