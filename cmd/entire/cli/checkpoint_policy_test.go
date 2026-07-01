package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestCheckpointPolicyCmd_PrintsDefaults(t *testing.T) {
	_, _ = setupCheckpointPolicyRepo(t)

	stdout, err := executeCheckpointPolicyCmd(t)
	require.NoError(t, err)
	require.Contains(t, stdout, "checkpoint_version: 1.0.0 (default)")
	require.Contains(t, stdout, "source: defaults")
}

func TestCheckpointPolicyCmd_HelpDocumentsEnforcementBehavior(t *testing.T) {
	t.Parallel()

	cmd := newCheckpointGroupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"policy", "--help"})

	err := cmd.Execute()
	require.NoError(t, err)

	help := stdout.String()
	require.Contains(t, help, "checkpoint_version selects the checkpoint metadata format used for new writes")
	require.Contains(t, help, `If another client configures a checkpoint_version expression this CLI cannot satisfy`)
	require.Contains(t, help, "commands that create checkpoint data fail until the CLI is upgraded")
	require.Contains(t, help, `Set checkpoint_version to "" to inherit the CLI default`)
	require.Contains(t, help, "Unsetting checkpoint_version still uses the normal downgrade guard")
	require.NotContains(t, help, "unset-checkpoint-version")
	require.NotContains(t, help, "checkpoint_min_version")
}

func TestCheckpointPolicyCmd_RejectsUnsupportedVersion(t *testing.T) {
	_, _ = setupCheckpointPolicyRepo(t)

	_, err := executeCheckpointPolicyCmd(t, "--checkpoint-version", "branch-v2342")
	require.ErrorContains(t, err, `checkpoint_version "branch-v2342" is not a valid SemVer constraint`)
}

func TestCheckpointPolicyCmd_PrintsUnsupportedConfiguredVersion(t *testing.T) {
	dir, bareDir := setupCheckpointPolicyRepo(t)
	seedCheckpointPolicyForCommand(t, dir, checkpointpolicy.Policy{
		CheckpointVersion: unsupportedCheckpointPolicyExpr,
	})
	pushCheckpointPolicyRefForCommandTest(t, dir, bareDir)

	stdout, err := executeCheckpointPolicyCmd(t)
	require.NoError(t, err)
	require.Contains(t, stdout, "checkpoint_version: "+unsupportedCheckpointPolicyExpr+" (unsupported)")
	require.NotContains(t, stdout, "writing branch-v1")
	require.NotContains(t, stdout, "checkpoint_min_version")
}

func TestCheckpointPolicyCmd_RejectsDowngradeWithoutForce(t *testing.T) {
	dir, bareDir := setupCheckpointPolicyRepo(t)
	seedCheckpointPolicyForCommand(t, dir, checkpointpolicy.Policy{
		CheckpointVersion: unsupportedCheckpointPolicyExpr,
	})
	pushCheckpointPolicyRefForCommandTest(t, dir, bareDir)

	_, err := executeCheckpointPolicyCmd(t, "--checkpoint-version", "1.0.0")
	require.ErrorContains(t, err, `checkpoint_version existing value "`+unsupportedCheckpointPolicyExpr+`"`)
}

func TestCheckpointPolicyCmd_UpdatesAndPushesOnlyPolicyRef(t *testing.T) {
	dir, bareDir := setupCheckpointPolicyRepo(t)
	testutil.WriteFile(t, dir, "README.md", "hello\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")

	stdout, err := executeCheckpointPolicyCmd(t, "--checkpoint-version", "^1.0.0")
	require.NoError(t, err)
	require.Contains(t, stdout, "checkpoint_version: ^1.0.0")
	require.Contains(t, stdout, "source: remote")

	remoteHash := checkpointPolicyRemoteHashForCommandTest(t, dir, bareDir)
	require.False(t, remoteHash.IsZero())

	repo := openCheckpointPolicyRepoForCommandTest(t, dir)
	localState, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, remoteHash, localState.Hash)

	branches := runCheckpointPolicyGit(t, dir, "ls-remote", bareDir, "refs/heads/*")
	require.Empty(t, strings.TrimSpace(branches))
}

func TestCheckpointPolicyCmd_UnsetsPolicyFields(t *testing.T) {
	dir, bareDir := setupCheckpointPolicyRepo(t)
	seedCheckpointPolicyForCommand(t, dir, checkpointpolicy.DefaultPolicy())
	pushCheckpointPolicyRefForCommandTest(t, dir, bareDir)

	stdout, err := executeCheckpointPolicyCmd(t, "--checkpoint-version", "")
	require.NoError(t, err)
	require.Contains(t, stdout, "checkpoint_version: 1.0.0 (default)")

	repo := openCheckpointPolicyRepoForCommandTest(t, dir)
	localState, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.NoError(t, err)
	require.Empty(t, localState.Policy)
	require.Equal(t, checkpointpolicy.DefaultPolicy(), checkpointpolicy.Normalize(localState.Policy))
}

func TestCheckpointPolicyCmd_SilencesContextCanceled(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)

	err := runCheckpointPolicy(cmd, checkpointPolicyOptions{})
	require.ErrorIs(t, err, context.Canceled)
	var silent *SilentError
	require.ErrorAs(t, err, &silent, "error = %T %v, want SilentError", err, err)
	require.Empty(t, stderr.String())
}

func TestCheckpointPolicyErrorSilencesWrappedContextCanceled(t *testing.T) {
	err := checkpointPolicyError("sync checkpoint policy", fmt.Errorf("remote: %w", context.Canceled))
	require.ErrorIs(t, err, context.Canceled)
	var silent *SilentError
	require.ErrorAs(t, err, &silent, "error = %T %v, want SilentError", err, err)
}

func setupCheckpointPolicyRepo(t *testing.T) (string, string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)

	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)
	runCheckpointPolicyGit(t, dir, "remote", "add", "origin", bareDir)
	return dir, bareDir
}

func executeCheckpointPolicyCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newCheckpointGroupCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"policy"}, args...))
	cmd.SetContext(t.Context())
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return stdout.String(), err
}

func seedCheckpointPolicyForCommand(t *testing.T, dir string, policy checkpointpolicy.Policy) plumbing.Hash {
	t.Helper()
	repo := openCheckpointPolicyRepoForCommandTest(t, dir)
	hash, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, policy)
	require.NoError(t, err)
	return hash
}

func openCheckpointPolicyRepoForCommandTest(t *testing.T, dir string) *git.Repository {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})
	return repo
}

func pushCheckpointPolicyRefForCommandTest(t *testing.T, dir, remote string) {
	t.Helper()
	refspec := checkpointpolicy.RefName.String() + ":" + checkpointpolicy.RefName.String()
	runCheckpointPolicyGit(t, dir, "push", remote, refspec)
}

func checkpointPolicyRemoteHashForCommandTest(t *testing.T, dir, remote string) plumbing.Hash {
	t.Helper()
	output := runCheckpointPolicyGit(t, dir, "ls-remote", remote, checkpointpolicy.RefName.String())
	fields := strings.Fields(output)
	require.NotEmpty(t, fields, "missing remote checkpoint policy ref")
	return plumbing.NewHash(fields[0])
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
