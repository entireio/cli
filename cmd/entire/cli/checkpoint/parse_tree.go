package checkpoint

import (
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TreeChange represents a single file change within a tree.
// Use a nil Entry to indicate deletion.
type TreeChange struct {
	// Path is the full path within the tree (e.g., "src/pkg/handler.go").
	Path string
	// Entry is the new tree entry. Nil means delete the file at Path.
	Entry *object.TreeEntry
}

// MergeMode controls how entries at the leaf directory are handled by UpdateSubtree.
type MergeMode int

const (
	// ReplaceAll replaces the entire leaf directory contents with the new entries.
	ReplaceAll MergeMode = iota
	// MergeKeepExisting merges new entries into the existing leaf directory,
	// keeping existing entries that are not overwritten (unless in DeleteNames).
	MergeKeepExisting
)

// UpdateSubtreeOptions configures the behavior of UpdateSubtree.
type UpdateSubtreeOptions struct {
	// MergeMode controls how entries at the leaf directory are handled.
	MergeMode MergeMode
	// DeleteNames lists entry names (at the leaf directory level) to delete.
	// Only applicable when MergeMode is MergeKeepExisting.
	DeleteNames []string
}

// UpdateSubtree replaces or creates a subtree at the given path within an existing tree.
// It walks the tree path, replacing only the entries along the modified path.
// All sibling entries at each level retain their original hashes — no re-reading needed.
//
// pathSegments is the directory path split into segments (e.g., ["a3", "b2c4d5e6f7"]).
// newEntries are the files/dirs to place at the leaf directory.
// Returns the new root tree hash.
func UpdateSubtree(
	repo *git.Repository,
	rootTreeHash plumbing.Hash,
	pathSegments []string,
	newEntries []object.TreeEntry,
	opts UpdateSubtreeOptions,
) (plumbing.Hash, error) {
	if len(pathSegments) == 0 {
		return buildLeafTree(repo, rootTreeHash, newEntries, opts)
	}

	// Read the current tree at this level
	// Get all entries in the tree and add them to the currentEntries slice
	var currentEntries []object.TreeEntry
	if rootTreeHash != plumbing.ZeroHash {
		tree, err := repo.TreeObject(rootTreeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to read tree %s: %w", rootTreeHash, err)
		}
		currentEntries = tree.Entries
	}

	targetDir := pathSegments[0]
	remainingPath := pathSegments[1:]

	// Find the existing subtree entry for targetDir
	var existingSubtreeHash plumbing.Hash
	found := false
	for _, entry := range currentEntries {
		if entry.Name == targetDir && entry.Mode == filemode.Dir {
			existingSubtreeHash = entry.Hash
			found = true
			break
		}
	}

	// Recurse into the subtree
	newSubtreeHash, err := UpdateSubtree(repo, existingSubtreeHash, remainingPath, newEntries, opts)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	// Build a new tree at this level with the updated subtree entry
	updatedEntries := make([]object.TreeEntry, 0, len(currentEntries)+1)
	replaced := false
	for _, entry := range currentEntries {
		if entry.Name == targetDir {
			updatedEntries = append(updatedEntries, object.TreeEntry{
				Name: targetDir,
				Mode: filemode.Dir,
				Hash: newSubtreeHash,
			})
			replaced = true
		} else {
			updatedEntries = append(updatedEntries, entry)
		}
	}
	if !found && !replaced {
		updatedEntries = append(updatedEntries, object.TreeEntry{
			Name: targetDir,
			Mode: filemode.Dir,
			Hash: newSubtreeHash,
		})
	}

	sortTreeEntries(updatedEntries)
	return storeTree(repo, updatedEntries)
}

// buildLeafTree builds the tree at the leaf of the UpdateSubtree path.
func buildLeafTree(
	repo *git.Repository,
	existingTreeHash plumbing.Hash,
	newEntries []object.TreeEntry,
	opts UpdateSubtreeOptions,
) (plumbing.Hash, error) {
	if opts.MergeMode == ReplaceAll || existingTreeHash == plumbing.ZeroHash {
		sorted := make([]object.TreeEntry, len(newEntries))
		copy(sorted, newEntries)
		sortTreeEntries(sorted)
		return storeTree(repo, sorted)
	}

	// MergeKeepExisting: read existing tree, merge
	existingTree, err := repo.TreeObject(existingTreeHash)
	if err != nil {
		sorted := make([]object.TreeEntry, len(newEntries))
		copy(sorted, newEntries)
		sortTreeEntries(sorted)
		return storeTree(repo, sorted)
	}

	// Build lookup of new entries by name
	newByName := make(map[string]object.TreeEntry, len(newEntries))
	for _, e := range newEntries {
		newByName[e.Name] = e
	}

	// Build delete set
	deleteSet := make(map[string]bool, len(opts.DeleteNames))
	for _, name := range opts.DeleteNames {
		deleteSet[name] = true
	}

	// Merge: keep existing unless overwritten or deleted
	merged := make([]object.TreeEntry, 0, len(existingTree.Entries)+len(newEntries))
	for _, existing := range existingTree.Entries {
		if deleteSet[existing.Name] {
			continue
		}
		if replacement, ok := newByName[existing.Name]; ok {
			merged = append(merged, replacement)
			delete(newByName, existing.Name)
		} else {
			merged = append(merged, existing)
		}
	}
	// Add remaining new entries (ones that didn't replace an existing entry)
	for _, e := range newEntries {
		if _, stillNew := newByName[e.Name]; stillNew {
			merged = append(merged, e)
		}
	}

	sortTreeEntries(merged)
	return storeTree(repo, merged)
}

// storeTree creates a git tree object from entries and stores it in the repo.
func storeTree(repo *git.Repository, entries []object.TreeEntry) (plumbing.Hash, error) {
	tree := &object.Tree{Entries: entries}
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

// ApplyTreeChanges applies multiple file-level changes to a tree efficiently.
// Changes are grouped by directory and applied in a single recursive pass.
// Unchanged subdirectories retain their hashes — this is the key optimization
// over FlattenTree + BuildTreeFromEntries for sparse changes.
func ApplyTreeChanges(
	repo *git.Repository,
	rootTreeHash plumbing.Hash,
	changes []TreeChange,
) (plumbing.Hash, error) {
	if len(changes) == 0 {
		return rootTreeHash, nil
	}

	// Read the current root tree
	var currentEntries []object.TreeEntry
	if rootTreeHash != plumbing.ZeroHash {
		tree, err := repo.TreeObject(rootTreeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to read tree: %w", err)
		}
		currentEntries = make([]object.TreeEntry, len(tree.Entries))
		copy(currentEntries, tree.Entries)
	}

	// Group changes by first path segment
	type dirChanges struct {
		subChanges []TreeChange // Changes within this subdirectory
		fileChange *TreeChange  // Direct file change at this level (nil if none)
	}
	grouped := make(map[string]*dirChanges)

	for i := range changes {
		c := changes[i]
		first, rest := splitFirstSegment(c.Path)
		if grouped[first] == nil {
			grouped[first] = &dirChanges{}
		}
		if rest == "" {
			cc := c
			grouped[first].fileChange = &cc
		} else {
			grouped[first].subChanges = append(grouped[first].subChanges, TreeChange{
				Path:  rest,
				Entry: c.Entry,
			})
		}
	}

	// Build entry map for modifications
	entryMap := make(map[string]object.TreeEntry, len(currentEntries))
	for _, e := range currentEntries {
		entryMap[e.Name] = e
	}

	// Process each group
	for name, dc := range grouped {
		if dc.fileChange != nil {
			if dc.fileChange.Entry == nil {
				delete(entryMap, name)
			} else {
				entryMap[name] = object.TreeEntry{
					Name: name,
					Mode: dc.fileChange.Entry.Mode,
					Hash: dc.fileChange.Entry.Hash,
				}
			}
		}
		if len(dc.subChanges) > 0 {
			existingHash := plumbing.ZeroHash
			if existing, ok := entryMap[name]; ok && existing.Mode == filemode.Dir {
				existingHash = existing.Hash
			}
			newSubHash, err := ApplyTreeChanges(repo, existingHash, dc.subChanges)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("failed to apply changes in %s: %w", name, err)
			}
			entryMap[name] = object.TreeEntry{
				Name: name,
				Mode: filemode.Dir,
				Hash: newSubHash,
			}
		}
	}

	result := make([]object.TreeEntry, 0, len(entryMap))
	for _, e := range entryMap {
		result = append(result, e)
	}
	sortTreeEntries(result)
	return storeTree(repo, result)
}

// splitFirstSegment splits "a/b/c" into ("a", "b/c"), and "file.txt" into ("file.txt", "").
func splitFirstSegment(path string) (first, rest string) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// getSessionsBranchRef returns the sessions branch parent commit hash and root tree hash
// without flattening the tree.
func (s *GitStore) getSessionsBranchRef() (plumbing.Hash, plumbing.Hash, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get sessions branch reference: %w", err)
	}

	parentCommit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get commit object: %w", err)
	}

	return ref.Hash(), parentCommit.TreeHash, nil
}
