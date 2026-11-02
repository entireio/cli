package checkpoint

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Tree-building helpers shared by the persistent stores and by tests that
// assemble git trees from flattened path → entry maps.

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

// treeNode represents a node in our tree structure.
type treeNode struct {
	entries map[string]*treeNode // subdirectories
	files   []object.TreeEntry   // files in this directory
}

// BuildTreeFromEntries builds a proper git tree structure from flattened file entries.
// Exported for use by strategy package (push_common.go, session_test.go)
func BuildTreeFromEntries(ctx context.Context, repo *git.Repository, entries map[string]object.TreeEntry) (plumbing.Hash, error) {
	// Build a tree structure
	root := &treeNode{
		entries: make(map[string]*treeNode),
		files:   []object.TreeEntry{},
	}

	// Insert all entries into the tree structure
	for fullPath, entry := range entries {
		normalizedPath, err := normalizeGitTreePath(fullPath)
		if err != nil {
			logInvalidGitTreePath(ctx, "build tree entry", fullPath, err)
			continue
		}
		parts := strings.Split(normalizedPath, "/")
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
