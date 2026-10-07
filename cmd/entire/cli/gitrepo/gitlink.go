package gitrepo

import (
	"fmt"

	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// GitlinkPaths returns the subset of paths (relative to repoRoot) that are
// submodule gitlinks (mode 160000) in HEAD's tree or in the index, so a
// submodule added but not yet committed counts too. An unborn HEAD or an
// unreadable index only narrows what is checked.
func GitlinkPaths(repoRoot string, paths []string) (map[string]struct{}, error) {
	found := make(map[string]struct{})
	if len(paths) == 0 {
		return found, nil
	}
	repo, err := OpenPath(repoRoot)
	if err != nil {
		return found, fmt.Errorf("open repository for submodule check: %w", err)
	}
	defer repo.Close()

	var headTree *object.Tree
	if head, headErr := repo.Head(); headErr == nil {
		if commit, commitErr := repo.CommitObject(head.Hash()); commitErr == nil {
			headTree, _ = commit.Tree() //nolint:errcheck // a missing tree just skips the HEAD check
		}
	}
	idx, idxErr := repo.Storer.Index()
	for _, path := range paths {
		if headTree != nil {
			if entry, findErr := headTree.FindEntry(path); findErr == nil && entry.Mode == filemode.Submodule {
				found[path] = struct{}{}
				continue
			}
		}
		if idxErr == nil {
			if entry, entryErr := idx.Entry(path); entryErr == nil && entry.Mode == filemode.Submodule {
				found[path] = struct{}{}
			}
		}
	}
	return found, nil
}
