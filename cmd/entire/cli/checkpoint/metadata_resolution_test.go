package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestCheckpointMetadataResolutionPreservesCallerPoliciesAndObservesRepair(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	repo, err := gitrepo.OpenPath(repoRoot)
	require.NoError(t, err)
	defer repo.Close()

	// A successful resolution first, so the breakage below also proves no
	// memoized success is served once the metadata goes bad — the direction a
	// per-worktree cache used to get wrong.
	queue, err := PushQueueForRepo(context.Background(), repo)
	require.NoError(t, err)
	require.NotNil(t, queue)

	commonFile := filepath.Join(repoRoot, ".git", "commondir")
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))

	queue, err = PushQueueForRepo(context.Background(), repo)
	require.ErrorContains(t, err, "resolve git common dir for push queue")
	require.Nil(t, queue, "required queue metadata must fail closed")
	require.Nil(t, repoRedactCache(repo), "optional redaction cache must fail open")

	require.NoError(t, os.Remove(commonFile))
	queue, err = PushQueueForRepo(context.Background(), repo)
	require.NoError(t, err, "a metadata repair must be observed without clearing a cache")
	require.NotNil(t, queue)
}

func TestWriteCheckpointFailsClosedOnMetadataError(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	repo, err := gitrepo.OpenPath(repoRoot)
	require.NoError(t, err)
	defer repo.Close()

	commonFile := filepath.Join(repoRoot, ".git", "commondir")
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))

	store := newEphemeralStore(repo, DefaultV1Refs())
	_, err = store.writeCheckpoint(context.Background(), WriteEphemeralOptions{
		SessionID:  "metadata-error-session",
		BaseCommit: "0123456789abcdef",
	})
	require.ErrorContains(t, err, "failed to resolve repo dirs",
		"the shadow-ref write path must fail closed on metadata errors")
}

func TestPushQueueForRepoUsesLinkedWorktreeCommonDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	testutil.InitRepo(t, mainRoot)
	testutil.WriteFile(t, mainRoot, "initial.txt", "initial\n")
	testutil.GitAdd(t, mainRoot, "initial.txt")
	testutil.GitCommit(t, mainRoot, "initial")
	testutil.RunGit(t, mainRoot, "worktree", "add", "-b", "linked", linkedRoot)

	repo, err := gitrepo.OpenPath(linkedRoot)
	require.NoError(t, err)
	defer repo.Close()

	queue, err := PushQueueForRepo(context.Background(), repo)
	require.NoError(t, err)
	ref := plumbing.NewBranchReferenceName("entire/linked-worktree")
	require.NoError(t, queue.Enqueue(ref))

	refs, err := NewPushQueue(filepath.Join(mainRoot, ".git")).Drain()
	require.NoError(t, err)
	require.Equal(t, []plumbing.ReferenceName{ref}, refs)
}

func TestPushQueueForOpenRepositoryRunsNoGitMetadataSubprocess(t *testing.T) {
	// GIT_TRACE2_EVENT is process-global.
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	repo, err := gitrepo.OpenPath(repoRoot)
	require.NoError(t, err)
	defer repo.Close()

	tracePath := filepath.Join(t.TempDir(), "git-trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", tracePath)

	queue, err := PushQueueForRepo(context.Background(), repo)
	require.NoError(t, err)
	require.NotNil(t, queue)

	_, err = os.Stat(tracePath)
	require.ErrorIs(t, err, os.ErrNotExist,
		"resolving checkpoint metadata from an open repository must not invoke git")

	testutil.RunGit(t, repoRoot, "rev-parse", "--git-common-dir")
	trace, err := os.ReadFile(tracePath)
	require.NoError(t, err)
	require.Contains(t, string(trace), `"event":"start"`, "the Trace2 subprocess detector must observe Git")
}
