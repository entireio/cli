package checkpoint

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func buildFileTree(t *testing.T, repo *git.Repository, files ...string) plumbing.Hash {
	t.Helper()
	changes := make([]TreeChange, 0, len(files))
	for _, f := range files {
		changes = append(changes, TreeChange{Path: f, Entry: &object.TreeEntry{
			Name: f, Mode: filemode.Regular, Hash: storeBlob(t, repo, f),
		}})
	}
	hash, err := ApplyTreeChanges(context.Background(), repo, plumbing.ZeroHash, changes)
	require.NoError(t, err)
	return hash
}

func TestRemoveCheckpointSubtree_RemovesWholeSubtreeKeepsSiblings(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)
	root := buildFileTree(t, repo,
		"a1/b2c3d4e5f6/metadata.json",
		"a1/b2c3d4e5f6/0/full.jsonl",
		"a1/b2c3d4e5f6/1/full.jsonl", // a session another clone added
		"a1/ffffffffff/metadata.json",
		"b2/c3d4e5f6a1/metadata.json",
	)

	newRoot, removed, err := RemoveCheckpointSubtree(repo, root, id.MustCheckpointID("a1b2c3d4e5f6"))
	require.NoError(t, err)
	assert.True(t, removed)
	assert.Equal(t, map[string]bool{
		"a1/ffffffffff/metadata.json": true,
		"b2/c3d4e5f6a1/metadata.json": true,
	}, keysOf(flattenTreeHelper(t, repo, newRoot, "")))
}

func TestRemoveCheckpointSubtree_DropsEmptiedShard(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)
	root := buildFileTree(t, repo, "a1/b2c3d4e5f6/metadata.json", "b2/c3d4e5f6a1/metadata.json")

	newRoot, removed, err := RemoveCheckpointSubtree(repo, root, id.MustCheckpointID("a1b2c3d4e5f6"))
	require.NoError(t, err)
	assert.True(t, removed)
	for _, e := range mustTreeObject(t, repo, newRoot).Entries {
		assert.NotEqual(t, "a1", e.Name, "an emptied shard directory must not linger as an empty tree")
	}
}

func TestRemoveCheckpointSubtree_AbsentIsNoop(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)
	root := buildFileTree(t, repo, "b2/c3d4e5f6a1/metadata.json")

	newRoot, removed, err := RemoveCheckpointSubtree(repo, root, id.MustCheckpointID("a1b2c3d4e5f6"))
	require.NoError(t, err)
	assert.False(t, removed)
	assert.Equal(t, root, newRoot)
}

func keysOf(m map[string]plumbing.Hash) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
