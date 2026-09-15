package testutil

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// CommitFiles writes a commit whose tree is exactly files (slash-separated
// path → content), with the given parents, directly into repo's object store.
// No worktree or index is involved, so it can build history on a ref that is
// never checked out — the checkpoint branch, for instance — without disturbing
// the working tree. Every commit carries its full file set: pass the previous
// commit's files plus the additions to extend a cumulative branch.
func CommitFiles(t *testing.T, repo *git.Repository, parents []plumbing.Hash, files map[string][]byte, message string) plumbing.Hash {
	t.Helper()
	treeHash := writeTree(t, repo, files)
	sig := object.Signature{Name: "test", Email: "test@test.com", When: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(obj))
	hash, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	return hash
}

// writeTree encodes files as nested tree objects and returns the root hash.
func writeTree(t *testing.T, repo *git.Repository, files map[string][]byte) plumbing.Hash {
	t.Helper()
	blobs := make(map[string]plumbing.Hash, len(files))
	subdirs := make(map[string]map[string][]byte)
	for p, content := range files {
		dir, rest, nested := strings.Cut(p, "/")
		if !nested {
			obj := repo.Storer.NewEncodedObject()
			obj.SetType(plumbing.BlobObject)
			w, err := obj.Writer()
			require.NoError(t, err)
			_, err = w.Write(content)
			require.NoError(t, err)
			require.NoError(t, w.Close())
			hash, err := repo.Storer.SetEncodedObject(obj)
			require.NoError(t, err)
			blobs[p] = hash
			continue
		}
		if subdirs[dir] == nil {
			subdirs[dir] = make(map[string][]byte)
		}
		subdirs[dir][rest] = content
	}
	entries := make([]object.TreeEntry, 0, len(blobs)+len(subdirs))
	for name, hash := range blobs {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
	}
	for name, sub := range subdirs {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: writeTree(t, repo, sub)})
	}
	// git orders tree entries by name, directories sorting as if suffixed by "/".
	sort.Slice(entries, func(i, j int) bool {
		return treeSortKey(entries[i]) < treeSortKey(entries[j])
	})
	tree := &object.Tree{Entries: entries}
	obj := repo.Storer.NewEncodedObject()
	require.NoError(t, tree.Encode(obj))
	hash, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	return hash
}

func treeSortKey(e object.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}
	return e.Name
}
