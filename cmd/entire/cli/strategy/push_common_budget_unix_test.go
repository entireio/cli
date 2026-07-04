//go:build unix

package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// A per-attempt timeout regression would let push/fetch/retry each spend a full
// budget (~2x). A hanging GIT_SSH_COMMAND blocks until the shared budget cuts it off.
//
// Not parallel: uses t.Setenv and overrides checkpointPushBudget.
func TestDoPushRef_SharedBudget_BoundsTotalWallClock(t *testing.T) {
	const budget = 2 * time.Second
	restoreBudget := checkpointPushBudget
	checkpointPushBudget = budget
	t.Cleanup(func() { checkpointPushBudget = restoreBudget })

	// Invoked by git for the ssh:// URL via GIT_SSH_COMMAND; outlives the bound below.
	hangScript := filepath.Join(t.TempDir(), "hang.sh")
	require.NoError(t, os.WriteFile(hangScript, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	t.Setenv("GIT_SSH_COMMAND", hangScript)
	// With a token set, newCommand rewrites ssh:// to https:// and the hang never runs.
	t.Setenv("ENTIRE_CHECKPOINT_TOKEN", "")

	tmpDir := setupRepoWithCheckpointBranch(t)
	t.Chdir(tmpDir)

	restoreStderr := captureStderr(t)
	defer restoreStderr()

	// ssh:// so git invokes GIT_SSH_COMMAND for the transport.
	const target = "ssh://git@localhost/checkpoints.git"

	start := time.Now()
	err := doPushRef(context.Background(), target, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
	elapsed := time.Since(start)

	require.NoError(t, err, "doPushRef degrades gracefully on a stuck transport")

	// Upper bound: one shared budget; per-attempt regression would land at ~2x.
	require.Less(t, elapsed, 5*time.Second,
		"doPushRef should return at ~budget, not stack multiple full timeouts; took %s", elapsed)
	// Lower bound: confirm the push hung and was cut off by the budget, not failing
	// instantly (which would make the upper bound meaningless).
	require.GreaterOrEqual(t, elapsed, budget/2,
		"push should have run until the budget deadline; took %s", elapsed)
}

// F2 (git-refs slice): the per-checkpoint push path
// (pushCheckpointRefWithRecovery) wraps its initial push, fetch+replay, and
// retry in the SAME shared checkpointPushBudget as the v1 path. A stuck
// transport must therefore bound the total wall clock to ~one budget, not
// stack a full budget per attempt. This is the git-refs analogue of
// TestDoPushRef_SharedBudget_BoundsTotalWallClock (git-branch/v1).
//
// Not parallel: uses t.Setenv, t.Chdir, and overrides checkpointPushBudget.
func TestPushCheckpointRefWithRecovery_SharedBudget_BoundsTotalWallClock(t *testing.T) {
	const budget = 2 * time.Second
	restoreBudget := checkpointPushBudget
	checkpointPushBudget = budget
	t.Cleanup(func() { checkpointPushBudget = restoreBudget })

	hangScript := filepath.Join(t.TempDir(), "hang.sh")
	require.NoError(t, os.WriteFile(hangScript, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	t.Setenv("GIT_SSH_COMMAND", hangScript)
	t.Setenv("ENTIRE_CHECKPOINT_TOKEN", "")

	tmpDir := setupRepoWithCheckpointRef(t)
	t.Chdir(tmpDir)

	restoreStderr := captureStderr(t)
	defer restoreStderr()

	const target = "ssh://git@localhost/checkpoints.git"
	ref := plumbing.ReferenceName("refs/entire/checkpoints/ab/abcdef012345")

	start := time.Now()
	err := pushCheckpointRefWithRecovery(context.Background(), target, ref)
	elapsed := time.Since(start)

	// The recovery path surfaces the error (its caller keeps the ref queued);
	// what matters here is that it returns bounded by the shared budget.
	require.Error(t, err, "a stuck transport should surface an error from the recovery path")
	require.Less(t, elapsed, 5*time.Second,
		"pushCheckpointRefWithRecovery should return at ~budget, not stack per-attempt timeouts; took %s", elapsed)
	require.GreaterOrEqual(t, elapsed, budget/2,
		"the push should have run until the budget deadline; took %s", elapsed)
}

// setupRepoWithCheckpointRef builds a repo with one per-checkpoint ref
// (refs/entire/checkpoints/<shard>/<id>) pointing at HEAD, so the git-refs push
// path has a real local ref to push.
func setupRepoWithCheckpointRef(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/ab/abcdef012345")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())))
	return tmpDir
}
