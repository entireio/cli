package cli

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

// globallyTrackedRepoForDoctor: a repo with no repo-level settings, as cwd,
// with the global tier on and isolated HOME/agent binaries. Not parallel-safe.
func globallyTrackedRepoForDoctor(t *testing.T) string {
	t.Helper()
	isolatedUserHome(t)
	pretendAgentBinaries(t)
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	writeUserSettings(t, `{"global":{"enabled":true}}`)
	return dir
}

// TestCheckGlobalTracking_GitHookStates covers doctor's report-only git-hook
// tail for a globally tracked repo: plain drift, a deliberate worktree-resident
// core.hooksPath, and an unresolvable hooks dir.
func TestCheckGlobalTracking_GitHookStates(t *testing.T) {
	t.Run("hooks absent reports lazy reinstall", func(t *testing.T) {
		globallyTrackedRepoForDoctor(t)
		cmd, out := newTestCmd(t)
		checkGlobalTracking(cmd)
		require.Contains(t, out.String(), "git hooks not installed yet")
		require.Contains(t, out.String(), "installed by the next agent session or git hook activity")
	})

	t.Run("worktree-resident hooksPath is deliberate", func(t *testing.T) {
		dir := globallyTrackedRepoForDoctor(t)
		cfg := exec.CommandContext(t.Context(), "git", "config", "core.hooksPath", ".husky")
		cfg.Dir = dir
		cfg.Env = testutil.GitIsolatedEnv()
		outBytes, err := cfg.CombinedOutput()
		require.NoError(t, err, string(outBytes))

		cmd, out := newTestCmd(t)
		checkGlobalTracking(cmd)
		require.Contains(t, out.String(), "GIT HOOKS SKIPPED (core.hooksPath inside the worktree)")
		require.NotContains(t, out.String(), "git hooks not installed yet")
	})

	t.Run("probe error is reported as unverified", func(t *testing.T) {
		globallyTrackedRepoForDoctor(t)
		strategy.SetHooksDirProbeErrorForTesting(errors.New("boom"))
		t.Cleanup(func() { strategy.SetHooksDirProbeErrorForTesting(nil) })

		cmd, out := newTestCmd(t)
		checkGlobalTracking(cmd)
		require.Contains(t, out.String(), "GIT HOOK STATE UNVERIFIED")
		require.Contains(t, out.String(), "boom")
	})

	t.Run("tier off is silent", func(t *testing.T) {
		globallyTrackedRepoForDoctor(t)
		writeUserSettings(t, `{"global":{"enabled":false}}`)
		cmd, out := newTestCmd(t)
		checkGlobalTracking(cmd)
		require.Empty(t, out.String())
	})
}
