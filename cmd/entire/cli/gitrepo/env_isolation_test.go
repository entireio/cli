package gitrepo_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestEnvWithoutRepoOverrides_IsolatesRepositoryConfig(t *testing.T) {
	// Cannot run in parallel: Git selectors are process-global.
	target, decoy := t.TempDir(), t.TempDir()
	testutil.InitRepo(t, target)
	testutil.InitRepo(t, decoy)
	targetHooks := filepath.Join(target, "target-hooks")
	decoyHooks := filepath.Join(decoy, "decoy-hooks")
	testutil.RunGit(t, target, "config", "core.hooksPath", targetHooks)
	testutil.RunGit(t, decoy, "config", "core.hooksPath", decoyHooks)

	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))

	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = target
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	output, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, targetHooks, strings.TrimSpace(string(output)))
}
