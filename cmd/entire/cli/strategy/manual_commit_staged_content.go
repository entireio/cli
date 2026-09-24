package strategy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Staged-content comparisons: whether the index holds exactly the content a
// commit gave a file, the evidence that a commit's work is what is being
// committed again.

type stagedEntry struct {
	blob    plumbing.Hash
	deleted bool
}

// stagedBlobs maps each file staged against HEAD to its index state.
func stagedBlobs(ctx context.Context, repo *git.Repository) (map[string]stagedEntry, error) {
	files, err := getStagedFiles(ctx)
	if err != nil {
		return nil, err
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err //nolint:wrapcheck // caller only tests for nil
	}
	inIndex := make(map[string]plumbing.Hash, len(idx.Entries))
	for _, e := range idx.Entries {
		inIndex[e.Name] = e.Hash
	}
	staged := make(map[string]stagedEntry, len(files))
	for _, f := range files {
		if h, ok := inIndex[f]; ok {
			staged[f] = stagedEntry{blob: h}
		} else {
			// getStagedFiles established that this path differs from HEAD. Its
			// absence from the index therefore represents a staged deletion.
			staged[f] = stagedEntry{deleted: true}
		}
	}
	return staged, nil
}

// recommitsFilesOf reports whether a file c changed is staged with the content
// it has in tree.
func recommitsFilesOf(c *object.Commit, tree *object.Tree, staged map[string]stagedEntry) bool {
	commitTree, err := c.Tree()
	if err != nil {
		return false
	}
	parent, err := c.Parents().Next()
	if err != nil {
		return false
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return false
	}
	changes, err := parentTree.Diff(commitTree)
	if err != nil {
		return false
	}
	for _, change := range changes {
		// A rename can be partially staged as either the destination addition
		// or the source deletion, so both endpoints can independently prove
		// that part of the dropped commit is being redone.
		for _, path := range []string{change.To.Name, change.From.Name} {
			if path == "" {
				continue
			}
			entry, ok := staged[path]
			if !ok {
				continue
			}
			origEntry, absent, err := findTreeEntry(tree, path)
			if entry.deleted && err == nil && absent {
				return true
			}
			if err == nil && !absent && origEntry.Hash.Equal(entry.blob) {
				return true
			}
		}
	}
	return false
}

// findTreeEntry distinguishes an absent path from a tree-read failure. Walking
// one component at a time also makes a non-directory ancestor an ordinary
// absence instead of asking go-git to decode that blob as a tree.
func findTreeEntry(tree *object.Tree, path string) (*object.TreeEntry, bool, error) {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		entry, err := tree.FindEntry(part)
		if errors.Is(err, object.ErrEntryNotFound) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("find tree entry %q: %w", part, err)
		}
		if i == len(parts)-1 {
			return entry, false, nil
		}
		if entry.Mode != filemode.Dir {
			return nil, true, nil
		}
		tree, err = tree.Tree(part)
		if err != nil {
			return nil, false, fmt.Errorf("open tree %q: %w", part, err)
		}
	}
	return nil, true, nil
}
