package strategy

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestPrePushSkipsCheckpointPushWhenPolicyWriteUnsupported(t *testing.T) {
	workDir := setupRepoWithCheckpointBranch(t)
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "remote", "add", "origin", bareDir)

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})
	_, err = checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "branch-v1",
	})
	require.NoError(t, err)

	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	t.Setenv(interactive.EnvTestTTY, "1")
	oldWriter := stderrWriter
	var stderr bytes.Buffer
	stderrWriter = &stderr
	t.Cleanup(func() { stderrWriter = oldWriter })

	err = NewManualCommitStrategy().PrePush(context.Background(), "origin")
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "requires checkpoint support newer than this Entire CLI")

	out := runCheckpointPolicyGit(t, workDir, "ls-remote", bareDir, "refs/heads/"+paths.MetadataBranchName)
	require.Empty(t, strings.TrimSpace(out))
}

func TestPrePushSkipsCheckpointPushWhenPolicyDiverged(t *testing.T) {
	workDir := setupRepoWithCheckpointBranch(t)
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "remote", "add", "origin", bareDir)

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})
	baseHash, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "push", bareDir, checkpointpolicy.RefName.String()+":"+checkpointpolicy.RefName.String())

	localHash, err := checkpointpolicy.WriteLocal(t.Context(), repo, baseHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	_, err = checkpointpolicy.WriteLocal(t.Context(), repo, baseHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "branch-v1",
	})
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "push", bareDir, checkpointpolicy.RefName.String()+":"+checkpointpolicy.RefName.String())
	require.NoError(t, checkpointpolicy.SetRef(repo, checkpointpolicy.RefName, localHash))

	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	t.Setenv(interactive.EnvTestTTY, "1")
	oldWriter := stderrWriter
	var stderr bytes.Buffer
	stderrWriter = &stderr
	t.Cleanup(func() { stderrWriter = oldWriter })

	err = NewManualCommitStrategy().PrePush(context.Background(), "origin")
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "Could not reconcile checkpoint policy")

	out := runCheckpointPolicyGit(t, workDir, "ls-remote", bareDir, "refs/heads/"+paths.MetadataBranchName)
	require.Empty(t, strings.TrimSpace(out))
}

func runCheckpointPolicyGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}
