package checkpoint

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// RemoveCheckpointSubtree returns rootTreeHash with the checkpoint's whole
// v1 subtree (<shard>/<rest>, cid.Path()) removed, and whether anything was
// removed. Removing the directory entry, rather than the files a local read
// knows about, also drops session directories another clone added under the
// same ID. A shard left empty is removed too, so no empty tree lingers.
// Sibling entries keep their hashes.
func RemoveCheckpointSubtree(repo *git.Repository, rootTreeHash plumbing.Hash, cid id.CheckpointID) (plumbing.Hash, bool, error) {
	segments := strings.Split(cid.Path(), "/")
	if cid.Kind() == id.KindUnknown || len(segments) != 2 {
		return plumbing.ZeroHash, false, fmt.Errorf("remove checkpoint subtree: invalid checkpoint ID %q", cid)
	}
	return removeTreePath(repo, rootTreeHash, segments)
}

func removeTreePath(repo *git.Repository, treeHash plumbing.Hash, segments []string) (plumbing.Hash, bool, error) {
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("read tree %s: %w", treeHash, err)
	}
	entries := make([]object.TreeEntry, 0, len(tree.Entries))
	removed := false
	for _, entry := range tree.Entries {
		if entry.Name != segments[0] || entry.Mode != filemode.Dir {
			entries = append(entries, entry)
			continue
		}
		if len(segments) == 1 {
			removed = true
			continue
		}
		childHash, childRemoved, err := removeTreePath(repo, entry.Hash, segments[1:])
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		if !childRemoved {
			entries = append(entries, entry)
			continue
		}
		removed = true
		if childHash == plumbing.ZeroHash {
			continue // the child directory is now empty: drop it
		}
		entries = append(entries, object.TreeEntry{Name: entry.Name, Mode: filemode.Dir, Hash: childHash})
	}
	if !removed {
		return treeHash, false, nil
	}
	if len(entries) == 0 {
		return plumbing.ZeroHash, true, nil
	}
	hash, err := storeTree(repo, entries)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	return hash, true, nil
}

// UpdatePersistentRef updates refName under the same writer lock and CAS
// retry loop Entire's checkpoint writers use. build runs again after every
// conflict, so it must read the current tip itself and return the hash it
// expects the ref to hold. A build error stops the update and is returned.
func UpdatePersistentRef(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, build func() (newHash, expectedHash plumbing.Hash, err error)) error {
	return updatePersistentRef(ctx, repo, refName, build)
}
