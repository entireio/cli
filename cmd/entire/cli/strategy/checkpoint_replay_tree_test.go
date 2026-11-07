package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestReplayOntoRemote_TreeChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		base, local, remote map[string]string
		want                map[string]string
		conflict            bool
	}{
		{name: "different additions", local: map[string]string{"1/metadata.json": "local"}, remote: map[string]string{"1/metadata.json": "remote"}, conflict: true},
		{name: "different modifications", base: map[string]string{"metadata.json": "base"}, local: map[string]string{"metadata.json": "local"}, remote: map[string]string{"metadata.json": "remote"}, conflict: true},
		{name: "delete modified file", base: map[string]string{"0/full.jsonl": "base"}, remote: map[string]string{"0/full.jsonl": "remote"}, conflict: true},
		{name: "modify deleted file", base: map[string]string{"0/full.jsonl": "base"}, local: map[string]string{"0/full.jsonl": "local"}, conflict: true},
		{name: "remote directory blocks local file", local: map[string]string{"1": "local"}, remote: map[string]string{"1/metadata.json": "remote"}, conflict: true},
		{name: "remote file blocks local directory", local: map[string]string{"1/metadata.json": "local"}, remote: map[string]string{"1": "remote"}, conflict: true},
		{name: "directory replacement loses remote change", base: map[string]string{"1/metadata.json": "base"}, local: map[string]string{"1": "local"}, remote: map[string]string{"1/metadata.json": "remote"}, conflict: true},
		{name: "identical additions", local: map[string]string{"1/metadata.json": "same"}, remote: map[string]string{"1/metadata.json": "same"}, want: map[string]string{"1/metadata.json": "same"}},
		{name: "identical modifications", base: map[string]string{"metadata.json": "base"}, local: map[string]string{"metadata.json": "same"}, remote: map[string]string{"metadata.json": "same"}, want: map[string]string{"metadata.json": "same"}},
		{name: "identical deletions", base: map[string]string{"0/full.jsonl": "base"}, local: map[string]string{"local": "local"}, remote: map[string]string{"remote": "remote"}, want: map[string]string{"local": "local", "remote": "remote"}},
		{name: "independent entries in new directory", local: map[string]string{"1/local": "local"}, remote: map[string]string{"1/remote": "remote"}, want: map[string]string{"1/local": "local", "1/remote": "remote"}},
		{name: "delete preserves new sibling", base: map[string]string{"1/old": "old"}, remote: map[string]string{"1/old": "old", "1/new": "new"}, want: map[string]string{"1/new": "new"}},
		{name: "replace unchanged file with directory", base: map[string]string{"1": "old"}, local: map[string]string{"1/new": "new"}, remote: map[string]string{"1": "old", "remote": "remote"}, want: map[string]string{"1/new": "new", "remote": "remote"}},
		{name: "replace unchanged directory with file", base: map[string]string{"1/old": "old"}, local: map[string]string{"1": "new"}, remote: map[string]string{"1/old": "old", "remote": "remote"}, want: map[string]string{"1": "new", "remote": "remote"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newCheckpointReplayRepo(t)
			base := makeTreeCommit(t, repo, nil, "base", tt.base)
			local := makeTreeCommit(t, repo, []plumbing.Hash{base}, "local", tt.local)
			remote := makeTreeCommit(t, repo, []plumbing.Hash{base}, "remote", tt.remote)
			tip, _, err := replayOntoRemote(t.Context(), repo, local, remote)
			if tt.conflict {
				require.ErrorIs(t, err, checkpoint.ErrReplayConflict)
				require.True(t, tip.IsZero())
				return
			}
			require.NoError(t, err)
			assertReplayFiles(t, repo, tip, tt.want)
			if tt.name == "identical additions" || tt.name == "identical modifications" {
				require.Equal(t, remote, tip, "already-applied changes must not add commits")
			}
		})
	}
}

func TestReplayOntoRemote_FileModes(t *testing.T) {
	t.Parallel()
	for _, remoteContent := range []string{"base", "remote modification"} {
		t.Run(remoteContent, func(t *testing.T) {
			t.Parallel()
			repo := newCheckpointReplayRepo(t)
			base := makeTreeCommit(t, repo, nil, "base", map[string]string{"file": "base"})
			baseCommit, err := repo.CommitObject(base)
			require.NoError(t, err)
			tree, err := baseCommit.Tree()
			require.NoError(t, err)
			entry, err := tree.FindEntry("file")
			require.NoError(t, err)
			entry.Mode = filemode.Executable
			modeTree, err := checkpoint.ApplyTreeChanges(t.Context(), repo, tree.Hash, []checkpoint.TreeChange{{Path: "file", Entry: entry}})
			require.NoError(t, err)
			local := makeOrphanCommit(t, repo, modeTree, []plumbing.Hash{base}, "local mode")
			remote := makeTreeCommit(t, repo, []plumbing.Hash{base}, "remote", map[string]string{"file": remoteContent})
			tip, _, err := replayOntoRemote(t.Context(), repo, local, remote)
			if remoteContent != "base" {
				require.ErrorIs(t, err, checkpoint.ErrReplayConflict)
				return
			}
			require.NoError(t, err)
			commit, err := repo.CommitObject(tip)
			require.NoError(t, err)
			gotTree, err := commit.Tree()
			require.NoError(t, err)
			got, err := gotTree.FindEntry("file")
			require.NoError(t, err)
			require.Equal(t, filemode.Executable, got.Mode)
		})
	}
}

func TestSafelyAdvanceLocalRef_ReplayConflictPreservesRef(t *testing.T) {
	t.Parallel()
	for _, disconnected := range []bool{false, true} {
		name := "connected"
		if disconnected {
			name = "disconnected"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := newCheckpointReplayRepo(t)
			base := makeEmptyTreeCommit(t, repo, nil, "base")
			parents := []plumbing.Hash{base}
			if disconnected {
				parents = nil
			}
			local := makeTreeCommit(t, repo, parents, "local", map[string]string{"checkpoint/metadata.json": "local"})
			remote := makeTreeCommit(t, repo, parents, "remote", map[string]string{"checkpoint/metadata.json": "remote"})
			forceSetTestRef(t, repo, local)
			err := SafelyAdvanceLocalRef(t.Context(), repo, safelyAdvanceTestRef, remote)
			require.ErrorIs(t, err, checkpoint.ErrReplayConflict)
			require.Equal(t, local, readTestRef(t, repo))
		})
	}
}

func TestCherryPickOnto_ShallowBoundaryConflict(t *testing.T) {
	t.Parallel()
	repo := newCheckpointReplayRepo(t)
	base := makeTreeCommit(t, repo, nil, "unavailable parent", map[string]string{"metadata.json": "remote"})
	local := makeTreeCommit(t, repo, []plumbing.Hash{base}, "boundary", map[string]string{"metadata.json": "local"})
	remote := makeTreeCommit(t, repo, nil, "remote", map[string]string{"metadata.json": "remote"})
	commit, err := repo.CommitObject(local)
	require.NoError(t, err)
	_, err = cherryPickOnto(t.Context(), repo, remote, []*object.Commit{commit}, map[plumbing.Hash]bool{local: true})
	require.ErrorIs(t, err, checkpoint.ErrReplayConflict)
}

func newCheckpointReplayRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })
	return repo
}

func assertReplayFiles(t *testing.T, repo *git.Repository, tip plumbing.Hash, want map[string]string) {
	t.Helper()
	commit, err := repo.CommitObject(tip)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	got := make(map[string]string)
	err = tree.Files().ForEach(func(file *object.File) error {
		content, readErr := file.Contents()
		if readErr != nil {
			return readErr
		}
		got[file.Name] = content
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestReplayOntoRemote_RetryFinalizedTranscript(t *testing.T) {
	t.Parallel()
	repo := newCheckpointReplayRepo(t)
	base := makeTreeCommit(t, repo, nil, "seed", map[string]string{"0/full.jsonl": "base"})
	provisional := makeTreeCommit(t, repo, []plumbing.Hash{base}, "provisional", map[string]string{"0/full.jsonl": "provisional"})
	local := makeTreeCommit(t, repo, []plumbing.Hash{provisional}, "finalized", map[string]string{"0/full.jsonl": "final"})
	remote := makeTreeCommit(t, repo, []plumbing.Hash{base}, "other session", map[string]string{"0/full.jsonl": "base", "1/full.jsonl": "remote"})
	delivered, _, err := replayOntoRemote(t.Context(), repo, local, remote)
	require.NoError(t, err)
	retried, _, err := replayOntoRemote(t.Context(), repo, local, delivered)
	require.NoError(t, err, "retry of already delivered final transcript must succeed")
	require.Equal(t, delivered, retried, "a retry must not add commits")
	assertReplayFiles(t, repo, retried, map[string]string{"0/full.jsonl": "final", "1/full.jsonl": "remote"})
}

func TestReplayOntoRemote_MultiCommitConflict(t *testing.T) {
	t.Parallel()
	repo := newCheckpointReplayRepo(t)
	base := makeTreeCommit(t, repo, nil, "base", map[string]string{"file": "base"})
	provisional := makeTreeCommit(t, repo, []plumbing.Hash{base}, "provisional", map[string]string{"file": "provisional"})
	local := makeTreeCommit(t, repo, []plumbing.Hash{provisional}, "final", map[string]string{"file": "local final", "local": "new"})
	remote := makeTreeCommit(t, repo, []plumbing.Hash{base}, "remote", map[string]string{"file": "remote final"})
	_, _, err := replayOntoRemote(t.Context(), repo, local, remote)
	require.ErrorIs(t, err, checkpoint.ErrReplayConflict)
}

func TestReplayOntoRemote_RevertedLocalChanges(t *testing.T) {
	t.Parallel()
	repo := newCheckpointReplayRepo(t)
	base := makeTreeCommit(t, repo, nil, "base", map[string]string{"file": "base"})
	changed := makeTreeCommit(t, repo, []plumbing.Hash{base}, "change", map[string]string{"file": "temporary"})
	reverted := makeTreeCommit(t, repo, []plumbing.Hash{changed}, "revert", map[string]string{"file": "base"})
	local := makeTreeCommit(t, repo, []plumbing.Hash{reverted}, "new work", map[string]string{"file": "base", "local": "new"})
	remote := makeTreeCommit(t, repo, []plumbing.Hash{base}, "remote", map[string]string{"file": "remote"})
	merged, _, err := replayOntoRemote(t.Context(), repo, local, remote)
	require.NoError(t, err)
	assertReplayFiles(t, repo, merged, map[string]string{"file": "remote", "local": "new"})
}

func TestCherryPickOnto_ForkedHistoryKeepsConflict(t *testing.T) {
	t.Parallel()
	repo := newCheckpointReplayRepo(t)
	base := makeTreeCommit(t, repo, nil, "base", map[string]string{"file": "base"})
	local := makeTreeCommit(t, repo, []plumbing.Hash{base}, "local", map[string]string{"file": "local"})
	fork := makeTreeCommit(t, repo, []plumbing.Hash{base}, "other branch", map[string]string{"file": "base"})
	remote := makeTreeCommit(t, repo, []plumbing.Hash{base}, "remote", map[string]string{"file": "remote"})
	var commits []*object.Commit
	for _, hash := range []plumbing.Hash{local, fork} {
		commit, err := repo.CommitObject(hash)
		require.NoError(t, err)
		commits = append(commits, commit)
	}
	_, err := cherryPickOnto(t.Context(), repo, remote, commits, nil)
	require.ErrorIs(t, err, checkpoint.ErrReplayConflict, "the other branch does not include the local edit")
}
