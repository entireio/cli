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

func TestPolicyCheckpointCmd_PrintsDefaults(t *testing.T) {
	_, _ = setupPolicyCheckpointRepo(t)

	stdout, err := executePolicyCheckpointCmd(t)
	require.NoError(t, err)
	require.Contains(t, stdout, "checkpoint_version: branch-v1")
	require.Contains(t, stdout, "checkpoint_min_version: branch-v1")
	require.Contains(t, stdout, "source: defaults")
}

func TestPolicyCheckpointCmd_RejectsUnsupportedVersion(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "checkpoint version", args: []string{"--checkpoint-version", "refs-v1"}, wantErr: "not write-supported"},
		{name: "minimum version", args: []string{"--checkpoint-min-version", "refs-v1"}, wantErr: "not read-supported"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _ = setupPolicyCheckpointRepo(t)

			_, err := executePolicyCheckpointCmd(t, tt.args...)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestPolicyCheckpointCmd_RejectsDowngradeWithoutForce(t *testing.T) {
	dir, bareDir := setupPolicyCheckpointRepo(t)
	seedPolicyForCheckpointCommand(t, dir, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "refs-v1",
	})
	pushCheckpointPolicyRefForCommandTest(t, dir, bareDir)

	_, err := executePolicyCheckpointCmd(t, "--checkpoint-version", "branch-v1", "--checkpoint-min-version", "branch-v1")
	require.ErrorContains(t, err, "would downgrade checkpoint_version")
}

func TestPolicyCheckpointCmd_UpdatesAndPushesOnlyPolicyRef(t *testing.T) {
	dir, bareDir := setupPolicyCheckpointRepo(t)
	testutil.WriteFile(t, dir, "README.md", "hello\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")

	stdout, err := executePolicyCheckpointCmd(t, "--checkpoint-version", "branch-v1", "--checkpoint-min-version", "branch-v1")
	require.NoError(t, err)
	require.Contains(t, stdout, "checkpoint_version: branch-v1")
	require.Contains(t, stdout, "checkpoint_min_version: branch-v1")
	require.Contains(t, stdout, "source: remote")

	remoteHash := checkpointPolicyRemoteHashForCommandTest(t, dir, bareDir)
	require.False(t, remoteHash.IsZero())

	repo := openPolicyCheckpointRepoForCommandTest(t, dir)
	localState, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, remoteHash, localState.Hash)

	branches := runPolicyCheckpointGit(t, dir, "ls-remote", bareDir, "refs/heads/*")
	require.Empty(t, strings.TrimSpace(branches))
}

func TestPolicyCheckpointCmd_SilencesContextCanceled(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)

	err := runPolicyCheckpoint(cmd, policyCheckpointOptions{})
	require.ErrorIs(t, err, context.Canceled)
	var silent *SilentError
	require.ErrorAs(t, err, &silent, "error = %T %v, want SilentError", err, err)
	require.Empty(t, stderr.String())
}

func TestPolicyCheckpointErrorSilencesWrappedContextCanceled(t *testing.T) {
	err := policyCheckpointError("sync checkpoint policy", fmt.Errorf("remote: %w", context.Canceled))
	require.ErrorIs(t, err, context.Canceled)
	var silent *SilentError
	require.ErrorAs(t, err, &silent, "error = %T %v, want SilentError", err, err)
}

func setupPolicyCheckpointRepo(t *testing.T) (string, string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)

	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)
	runPolicyCheckpointGit(t, dir, "remote", "add", "origin", bareDir)
	return dir, bareDir
}

func executePolicyCheckpointCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newPolicyCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"checkpoint"}, args...))
	cmd.SetContext(t.Context())
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return stdout.String(), err
}

func seedPolicyForCheckpointCommand(t *testing.T, dir string, policy checkpointpolicy.Policy) plumbing.Hash {
	t.Helper()
	repo := openPolicyCheckpointRepoForCommandTest(t, dir)
	hash, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, policy)
	require.NoError(t, err)
	return hash
}

func openPolicyCheckpointRepoForCommandTest(t *testing.T, dir string) *git.Repository {
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
	runPolicyCheckpointGit(t, dir, "push", remote, refspec)
}

func checkpointPolicyRemoteHashForCommandTest(t *testing.T, dir, remote string) plumbing.Hash {
	t.Helper()
	output := runPolicyCheckpointGit(t, dir, "ls-remote", remote, checkpointpolicy.RefName.String())
	fields := strings.Fields(output)
	require.NotEmpty(t, fields, "missing remote checkpoint policy ref")
	return plumbing.NewHash(fields[0])
}

func runPolicyCheckpointGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}
