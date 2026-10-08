package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnableCmd_BareEnableHealsEmptyOrphanFromCheckpointRemote verifies issue
// #1374 finding 4: a bare `entire enable` on an already-configured, already-enabled
// repo must reach the checkpoint_remote heal. That path (runEnableOnConfiguredRepo)
// previously short-circuited on the "already enabled" branch and never called
// EnsureSetup, so a pre-fix empty orphan could never be recovered by the most
// natural recovery command.
//
// Not parallel: setupTestRepo uses t.Chdir and t.Setenv.
func TestEnableCmd_BareEnableHealsEmptyOrphanFromCheckpointRemote(t *testing.T) {
	// Checkpoint remote (device A): holds a real entire/checkpoints/v1 branch.
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefault := healTestCurrentBranch(t, remoteDir)
	healTestGit(t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	healTestGit(t, remoteDir, "rm", "-rf", ".")
	healTestWriteCheckpoint(t, remoteDir, "aaaaaaaaaaaa")
	healTestGit(t, remoteDir, "checkout", remoteDefault)
	remoteTip := healTestRevParse(t, remoteDir, paths.MetadataBranchName)

	// Local repo (device B): configured + enabled with a checkpoint_remote, but
	// carrying the pre-#1374 empty orphan. setupTestRepo inits the repo and chdirs
	// into it.
	setupTestRepo(t)
	localDir, err := os.Getwd()
	require.NoError(t, err)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	localDefault := healTestCurrentBranch(t, localDir)
	// SSH origin so remote.FetchURL derives the github checkpoint URL.
	healTestGit(t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")
	healTestGit(t, localDir, "checkout", "--orphan", paths.MetadataBranchName)
	healTestGit(t, localDir, "rm", "-rf", ".")
	healTestGit(t, localDir, "commit", "--allow-empty", "-m", "Initialize metadata ref")
	healTestGit(t, localDir, "checkout", localDefault)

	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	writeClaudeHooksFixture(t)

	// Redirect the derived checkpoint URL to the local checkpoint remote so the
	// real fetch path runs hermetically.
	healTestRedirectURL(t, localDir, "git@github.com:org/checkpoints.git", "file://"+remoteDir)
	paths.ClearWorktreeRootCache()

	orphanTip := healTestRevParse(t, localDir, paths.MetadataBranchName)
	require.NotEqual(t, remoteTip, orphanTip, "test setup: local orphan must differ from the checkpoint remote tip")

	cmd := newEnableCmd()
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute(), "bare enable should succeed")

	healedTip := healTestRevParse(t, localDir, paths.MetadataBranchName)
	assert.Equal(t, remoteTip, healedTip,
		"a bare `entire enable` must heal the empty orphan from the checkpoint remote")
	files := healTestMetadataFiles(t, localDir)
	assert.Contains(t, files, "aa/aaaaaaaaaa/"+paths.MetadataFileName,
		"the healed branch should contain the checkpoint remote data")
}

func healTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	testutil.RunGit(t, dir, args...)
}

func healTestCurrentBranch(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
}

func healTestRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", rev))
}

func healTestWriteCheckpoint(t *testing.T, dir, checkpointID string) {
	t.Helper()
	checkpointDir := filepath.Join(dir, checkpointID[:2], checkpointID[2:])
	require.NoError(t, os.MkdirAll(checkpointDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(checkpointDir, paths.MetadataFileName),
		[]byte(fmt.Sprintf(`{"checkpoint_id":%q}`, checkpointID)),
		0o644,
	))
	healTestGit(t, dir, "add", ".")
	healTestGit(t, dir, "commit", "-m", "Checkpoint: "+checkpointID)
}

func healTestMetadataFiles(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "ls-tree", "-r", "--name-only", "refs/heads/"+paths.MetadataBranchName)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return string(out)
}

func healTestRedirectURL(t *testing.T, repoDir, matchURL, replacementURL string) {
	t.Helper()
	configPath := filepath.Join(repoDir, ".git", "config")
	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	_, err = fmt.Fprintf(f, "\n[url %q]\n\tinsteadOf = %s\n", replacementURL, matchURL)
	require.NoError(t, err)
}
