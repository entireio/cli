//go:build unix

package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/stretchr/testify/require"
)

// A per-attempt timeout regression would let push/fetch/retry each spend a full
// budget (~2x). A hanging GIT_SSH_COMMAND blocks until the shared budget cuts it off.
//
// Not parallel: uses t.Setenv and overrides checkpointPushBudget.
func TestDoPushBranch_SharedBudget_BoundsTotalWallClock(t *testing.T) {
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
	err := doPushBranch(context.Background(), target, paths.MetadataBranchName)
	elapsed := time.Since(start)

	require.NoError(t, err, "doPushBranch degrades gracefully on a stuck transport")

	// Upper bound: one shared budget; per-attempt regression would land at ~2x.
	require.Less(t, elapsed, 5*time.Second,
		"doPushBranch should return at ~budget, not stack multiple full timeouts; took %s", elapsed)
	// Lower bound: confirm the push hung and was cut off by the budget, not failing
	// instantly (which would make the upper bound meaningless).
	require.GreaterOrEqual(t, elapsed, budget/2,
		"push should have run until the budget deadline; took %s", elapsed)
}
