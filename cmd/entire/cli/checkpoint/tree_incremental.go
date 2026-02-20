package checkpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// changeTree represents pending changes to apply to a git tree incrementally.
// Only changed paths are traversed; unchanged subtrees are reused by hash reference.
type changeTree struct {
	dirs  map[string]*changeTree // subdirectory changes
	files map[string]*fileChange // file changes in this directory
}

// fileChange represents a single file addition, update, or deletion.
type fileChange struct {
	hash   plumbing.Hash
	mode   filemode.FileMode
	delete bool
}

func newChangeTree() *changeTree {
	return &changeTree{
		dirs:  make(map[string]*changeTree),
		files: make(map[string]*fileChange),
	}
}

// addFile records a file addition or update in the change tree.
// The path uses forward slashes as separators (git convention).
func (ct *changeTree) addFile(path string, hash plumbing.Hash, mode filemode.FileMode) {
	parts := strings.Split(path, "/")
	node := ct
	for _, part := range parts[:len(parts)-1] {
		if _, ok := node.dirs[part]; !ok {
			node.dirs[part] = newChangeTree()
		}
		node = node.dirs[part]
	}
	node.files[parts[len(parts)-1]] = &fileChange{hash: hash, mode: mode}
}

// deleteFile records a file deletion in the change tree.
func (ct *changeTree) deleteFile(path string) {
	parts := strings.Split(path, "/")
	node := ct
	for _, part := range parts[:len(parts)-1] {
		if _, ok := node.dirs[part]; !ok {
			node.dirs[part] = newChangeTree()
		}
		node = node.dirs[part]
	}
	node.files[parts[len(parts)-1]] = &fileChange{delete: true}
}

// isEmpty returns true if the change tree has no pending changes.
func (ct *changeTree) isEmpty() bool {
	return len(ct.dirs) == 0 && len(ct.files) == 0
}

// applyChangesToTree merges a changeTree into a base git tree, returning the new root tree hash.
// Only directories containing changes are rebuilt; unchanged subtrees are reused by hash reference.
// This is O(changed_files * tree_depth) instead of O(total_files_in_repo).
func applyChangesToTree(repo *git.Repository, baseTreeHash plumbing.Hash, changes *changeTree) (plumbing.Hash, error) {
	if changes.isEmpty() {
		return baseTreeHash, nil
	}

	baseTree, err := repo.TreeObject(baseTreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get tree: %w", err)
	}

	// Track which change entries we've processed (matched to existing tree entries)
	processedDirs := make(map[string]bool)
	processedFiles := make(map[string]bool)

	var newEntries []object.TreeEntry

	// Process existing entries
	for _, entry := range baseTree.Entries {
		if entry.Mode == filemode.Dir {
			if dirChanges, ok := changes.dirs[entry.Name]; ok {
				// Recurse into subdirectory with changes
				newSubHash, subErr := applyChangesToTree(repo, entry.Hash, dirChanges)
				if subErr != nil {
					return plumbing.ZeroHash, subErr
				}
				newEntries = append(newEntries, object.TreeEntry{
					Name: entry.Name,
					Mode: filemode.Dir,
					Hash: newSubHash,
				})
				processedDirs[entry.Name] = true
			} else if fileChange, ok := changes.files[entry.Name]; ok {
				// Type conflict: base has dir, changes has file → replace dir with file
				if !fileChange.delete {
					newEntries = append(newEntries, object.TreeEntry{
						Name: entry.Name,
						Mode: fileChange.mode,
						Hash: fileChange.hash,
					})
				}
				processedFiles[entry.Name] = true
			} else {
				// No changes in this directory, keep as-is
				newEntries = append(newEntries, entry)
			}
		} else {
			// File entry
			if change, ok := changes.files[entry.Name]; ok {
				if !change.delete {
					newEntries = append(newEntries, object.TreeEntry{
						Name: entry.Name,
						Mode: change.mode,
						Hash: change.hash,
					})
				}
				// If delete: don't add to newEntries
				processedFiles[entry.Name] = true
			} else if dirChanges, ok := changes.dirs[entry.Name]; ok {
				// Type conflict: base has file, changes has dir → replace file with dir
				newSubHash, subErr := createTreeFromChanges(repo, dirChanges)
				if subErr != nil {
					return plumbing.ZeroHash, subErr
				}
				if newSubHash != plumbing.ZeroHash {
					newEntries = append(newEntries, object.TreeEntry{
						Name: entry.Name,
						Mode: filemode.Dir,
						Hash: newSubHash,
					})
				}
				processedDirs[entry.Name] = true
			} else {
				// No change, keep as-is
				newEntries = append(newEntries, entry)
			}
		}
	}

	// Add new files (not in base tree)
	for name, change := range changes.files {
		if !processedFiles[name] && !change.delete {
			newEntries = append(newEntries, object.TreeEntry{
				Name: name,
				Mode: change.mode,
				Hash: change.hash,
			})
		}
	}

	// Add new directories (not in base tree)
	for name, dirChanges := range changes.dirs {
		if !processedDirs[name] {
			newSubHash, subErr := createTreeFromChanges(repo, dirChanges)
			if subErr != nil {
				return plumbing.ZeroHash, subErr
			}
			// Skip empty trees (e.g., all changes were deletions for non-existent paths)
			if newSubHash == plumbing.ZeroHash {
				continue
			}
			newEntries = append(newEntries, object.TreeEntry{
				Name: name,
				Mode: filemode.Dir,
				Hash: newSubHash,
			})
		}
	}

	sortTreeEntries(newEntries)
	return storeTree(repo, newEntries)
}

// createTreeFromChanges creates a new tree from a changeTree with no base tree.
// Used for new directories that don't exist in the base tree.
func createTreeFromChanges(repo *git.Repository, changes *changeTree) (plumbing.Hash, error) {
	var entries []object.TreeEntry

	for name, change := range changes.files {
		if !change.delete {
			entries = append(entries, object.TreeEntry{
				Name: name,
				Mode: change.mode,
				Hash: change.hash,
			})
		}
	}

	for name, dirChanges := range changes.dirs {
		subHash, err := createTreeFromChanges(repo, dirChanges)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		// Skip empty subtrees (all files were deletions)
		if subHash == plumbing.ZeroHash {
			continue
		}
		entries = append(entries, object.TreeEntry{
			Name: name,
			Mode: filemode.Dir,
			Hash: subHash,
		})
	}

	// If all entries were deletions, don't create an empty tree
	if len(entries) == 0 {
		return plumbing.ZeroHash, nil
	}

	sortTreeEntries(entries)
	return storeTree(repo, entries)
}

// storeTree creates and stores a git tree object from entries.
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

// addDirectoryToChangeTree walks a filesystem directory and adds all files to a changeTree.
// dirPathAbs is the absolute path for reading files, dirPathRel is the git tree path prefix.
// Files are redacted before storage (JSONL-aware for transcripts, plain string for others).
// Symlinks are skipped to prevent capturing files outside the metadata directory.
func addDirectoryToChangeTree(repo *git.Repository, dirPathAbs, dirPathRel string, ct *changeTree) error {
	err := filepath.Walk(dirPathAbs, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		// Skip symlinks to prevent reading files outside the metadata directory.
		// A symlink could point to sensitive files (e.g., /etc/passwd) which would
		// then be captured in the checkpoint and stored in git history.
		// NOTE: filepath.Walk uses os.Stat (follows symlinks), so info.Mode() never
		// reports ModeSymlink. We use os.Lstat to check the entry itself.
		// This check MUST come before IsDir() because Walk follows symlinked
		// directories and would recurse into them otherwise.
		linfo, lstatErr := os.Lstat(path)
		if lstatErr != nil {
			return fmt.Errorf("failed to lstat %s: %w", path, lstatErr)
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			return nil
		}

		relWithinDir, relErr := filepath.Rel(dirPathAbs, path)
		if relErr != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, relErr)
		}

		// Prevent path traversal via symlinks pointing outside the metadata dir
		if strings.HasPrefix(relWithinDir, "..") {
			return fmt.Errorf("path traversal detected: %s", relWithinDir)
		}

		// Use forward slashes for git tree paths
		treePath := filepath.ToSlash(dirPathRel) + "/" + filepath.ToSlash(relWithinDir)

		// Use redacted blob creation to strip secrets from transcripts and other metadata
		blobHash, mode, blobErr := createRedactedBlobFromFile(repo, path, treePath)
		if blobErr != nil {
			return fmt.Errorf("failed to create blob for %s: %w", path, blobErr)
		}

		ct.addFile(treePath, blobHash, mode)
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk directory %s: %w", dirPathAbs, err)
	}
	return nil
}
