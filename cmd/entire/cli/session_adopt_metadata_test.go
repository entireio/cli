package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestAdoptStoreRequiresGitRepositoryValidation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o750))
	_, err := gitrepo.ResolveWorktreeMetadata(root)
	require.NoError(t, err, "directory metadata alone does not establish a Git repository")
	store, _, _, err := stateStoreForWorktree(context.Background(), root)
	require.ErrorContains(t, err, "resolve source git directory")
	require.Nil(t, store, "adoption must retain Git's semantic validation")
}

func TestAdoptSourceMetadataFailureFallsBackToWorktreePath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testutil.InitRepo(t, root)
	ctx := context.Background()
	store, err := session.NewStateStoreForWorktree(ctx, root)
	require.NoError(t, err)
	state := &session.State{
		SessionID:    "source-path-fallback",
		Phase:        session.PhaseActive,
		StartedAt:    time.Now(),
		WorktreePath: root,
	}
	require.NoError(t, store.Save(ctx, state))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "commondir"), []byte("missing\n"), 0o600))
	_, err = gitrepo.ResolveWorktreeMetadata(root)
	require.Error(t, err)
	selected, err := selectAdoptSourceSession(ctx, store, root, state.SessionID)
	require.NoError(t, err)
	require.Equal(t, state.SessionID, selected.SessionID)
}

func TestBuildAdoptedStateUsesBareBackedWorktreeMetadata(t *testing.T) {
	// buildAdoptedSessionState discovers its target from CWD.
	tmp := t.TempDir()
	seed := filepath.Join(tmp, "seed")
	storage := filepath.Join(tmp, "storage")
	linked := filepath.Join(tmp, "target")
	testutil.InitRepo(t, seed)
	testutil.WriteFile(t, seed, "initial.txt", "initial\n")
	testutil.GitAdd(t, seed, "initial.txt")
	testutil.GitCommit(t, seed, "initial")
	testutil.RunGit(t, tmp, "clone", "--bare", seed, storage)
	testutil.RunGit(t, tmp, "--git-dir", storage, "worktree", "add", "-b", "target", linked)
	t.Chdir(linked)

	state, _, err := buildAdoptedSessionState(context.Background(), &session.State{SessionID: "bare-adoption"})
	require.NoError(t, err)
	require.Equal(t, "target", state.WorktreeID)
	require.Equal(t, testutil.GetHeadHash(t, seed), state.BaseCommit)
}
