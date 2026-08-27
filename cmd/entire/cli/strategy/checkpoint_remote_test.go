package strategy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/vercelconfig"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  string
		want bool
	}{
		{"remote name", "origin", false},
		{"SSH SCP", "git@github.com:org/repo.git", true},
		{"HTTPS", "https://github.com/org/repo.git", true},
		{"SSH protocol", "ssh://git@github.com/org/repo.git", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, remote.IsURL(tt.val))
		})
	}
}

// Not parallel: uses t.Chdir()
func TestFetchBranchIfMissing_CreatesLocalFromRemote(t *testing.T) {
	ctx := context.Background()

	// Set up a "remote" repo with a branch
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	// Get the default branch name before switching
	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = remoteDir
	branchCmd.Env = testutil.GitIsolatedEnv()
	branchOut, err := branchCmd.Output()
	require.NoError(t, err)
	defaultBranch := strings.TrimSpace(string(branchOut))

	// Create an orphan branch in the remote repo (simulating entire/checkpoints/v1)
	cmd := exec.CommandContext(ctx, "git", "checkout", "--orphan", "entire/checkpoints/v1")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "rm", "-rf", ".")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, remoteDir, "metadata.json", `{"test": true}`)
	testutil.GitAdd(t, remoteDir, "metadata.json")

	cmd = exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-m", "checkpoint data")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Go back to the default branch
	cmd = exec.CommandContext(ctx, "git", "checkout", defaultBranch)
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Set up local repo
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	t.Chdir(localDir)

	// Verify branch doesn't exist locally
	assert.False(t, testutil.BranchExists(t, localDir, "entire/checkpoints/v1"))

	// Fetch using the remote dir as a URL (local path)
	require.NoError(t, fetchMetadataBranchIfMissing(ctx, remoteDir))

	// Verify the branch now exists locally
	assert.True(t, testutil.BranchExists(t, localDir, "entire/checkpoints/v1"))
}

// Not parallel: uses t.Chdir()
func TestFetchBranchIfMissing_NoOpWhenBranchExistsLocally(t *testing.T) {
	ctx := context.Background()

	// Set up local repo with the branch already existing
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Get the default branch name before switching
	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = localDir
	branchCmd.Env = testutil.GitIsolatedEnv()
	branchOut, err := branchCmd.Output()
	require.NoError(t, err)
	defaultBranch := strings.TrimSpace(string(branchOut))

	// Create the branch locally
	cmd := exec.CommandContext(ctx, "git", "checkout", "--orphan", "entire/checkpoints/v1")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "rm", "-rf", ".")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, localDir, "data.json", `{"local": true}`)
	testutil.GitAdd(t, localDir, "data.json")

	cmd = exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-m", "local checkpoint")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Switch back to the default branch
	cmd = exec.CommandContext(ctx, "git", "checkout", defaultBranch)
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	t.Chdir(localDir)

	// Should be a no-op since branch exists locally (no network call).
	// Use a nonexistent path — if it tried to fetch, it would fail.
	require.NoError(t, fetchMetadataBranchIfMissing(ctx, "/nonexistent/repo.git"))
}

// Not parallel: uses t.Chdir()
func TestFetchBranchIfMissing_NoOpWhenBranchNotOnRemote(t *testing.T) {
	ctx := context.Background()

	// Set up a "remote" repo without the checkpoint branch
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	// Set up local repo
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	t.Chdir(localDir)

	err := fetchMetadataBranchIfMissing(ctx, remoteDir)
	require.NoError(t, err)

	// Branch should still not exist locally
	assert.False(t, testutil.BranchExists(t, localDir, "entire/checkpoints/v1"))
}

// Not parallel: uses t.Chdir()
func TestResolvePushSettings_NoConfig(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// Create settings without checkpoint_remote
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true}`),
		0o644,
	))

	t.Chdir(tmpDir)

	ps := resolvePushSettings(t.Context(), "origin")
	assert.Equal(t, "origin", ps.pushTarget())
	assert.False(t, ps.hasCheckpointURL())
	assert.False(t, ps.pushDisabled)
}

// Not parallel: uses t.Chdir()
func TestResolvePushSettings_PushDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"push_sessions": false}}`),
		0o644,
	))

	t.Chdir(tmpDir)

	ps := resolvePushSettings(t.Context(), "origin")
	assert.Equal(t, "origin", ps.pushTarget())
	assert.True(t, ps.pushDisabled)
}

// Not parallel: uses t.Chdir()
func TestResolvePushSettings_WithCheckpointRemote_HTTPS(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Add origin with an HTTPS-style URL
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "https://github.com/org/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// Seed the local v1 metadata branch so resolvePushSettings finds it and
	// skips fetchMetadataBranchIfMissing. Without it the test fetches the
	// resolved checkpoint URL from github.com for real — slow, flaky, and it
	// triggers the OS keychain credential helper when GitHub returns 401.
	runCheckpointRemoteGit(ctx, t, localDir, "branch", paths.MetadataBranchName)

	t.Chdir(localDir)

	ps := resolvePushSettings(ctx, "origin")
	assert.True(t, ps.hasCheckpointURL())
	assert.Equal(t, "https://github.com/org/checkpoints.git", ps.pushTarget())
	assert.False(t, ps.pushDisabled)
}

// Not parallel: uses t.Chdir()
func TestResolvePushSettings_WithCheckpointRemote_SSH(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Add origin with SSH URL
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "git@github.com:org/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// Seed the local v1 metadata branch so resolvePushSettings finds it and
	// skips fetchMetadataBranchIfMissing. Without it the test fetches the
	// resolved checkpoint URL from github.com for real — slow, flaky, and it
	// triggers the OS keychain credential helper when GitHub returns 401.
	runCheckpointRemoteGit(ctx, t, localDir, "branch", paths.MetadataBranchName)

	t.Chdir(localDir)

	ps := resolvePushSettings(ctx, "origin")
	assert.True(t, ps.hasCheckpointURL())
	assert.Equal(t, "git@github.com:org/checkpoints.git", ps.pushTarget())
}

// Not parallel: uses t.Chdir()
func TestResolvePushSettings_ForkDetection(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Origin remote owner differs from the configured checkpoint remote owner.
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "git@github.com:alice/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	t.Chdir(localDir)

	ps := resolvePushSettings(ctx, "origin")
	// Should fall back to origin since the remote owner differs (alice != org).
	assert.False(t, ps.hasCheckpointURL())
	assert.Equal(t, "origin", ps.pushTarget())
	assert.False(t, ps.pushDisabled)
}

// Not parallel: uses t.Chdir()
//
// When origin is an entire:// push-through mirror whose forge (gh) matches the
// configured checkpoint provider (github), checkpoints route through the same
// cluster mirror instead of falling back to a direct github.com URL.
func TestResolvePushSettings_WithCheckpointRemote_EntireMirror(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Origin is an entire:// mirror on cluster app.entire.io for forge gh.
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "entire://app.entire.io/gh/org/main-repo")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// Seed the local v1 metadata branch so resolvePushSettings finds it and
	// skips fetchMetadataBranchIfMissing — which would otherwise invoke the
	// entire:// remote helper against a live cluster.
	runCheckpointRemoteGit(ctx, t, localDir, "branch", paths.MetadataBranchName)

	t.Chdir(localDir)

	ps := resolvePushSettings(ctx, "origin")
	assert.True(t, ps.hasCheckpointURL())
	// Keeps the cluster host and forge segment, swaps in the checkpoint repo.
	assert.Equal(t, "entire://app.entire.io/gh/org/checkpoints", ps.pushTarget())
	assert.False(t, ps.pushDisabled)
}

// Not parallel: uses t.Chdir()
//
// When origin is an entire:// mirror of a different forge (et) than the
// configured checkpoint provider (github), it must not route through the
// mirror; it falls back to the provider's canonical host.
func TestResolvePushSettings_EntireMirrorForgeMismatchFallsBackToProvider(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "entire://app.entire.io/et/org/main-repo")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// Seed the local v1 branch so the provider-host fallback URL isn't fetched
	// from github.com for real.
	runCheckpointRemoteGit(ctx, t, localDir, "branch", paths.MetadataBranchName)

	t.Chdir(localDir)

	ps := resolvePushSettings(ctx, "origin")
	assert.True(t, ps.hasCheckpointURL())
	// Provider host over SSH (default transport), not the non-matching mirror.
	assert.Equal(t, "git@github.com:org/checkpoints.git", ps.pushTarget())
	assert.False(t, ps.pushDisabled)
}

// Not parallel: uses t.Chdir()
func TestResolvePushSettings_CheckpointURLDoesNotAffectRemoteField(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Add origin with HTTPS URL
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "https://github.com/org/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// Seed the local v1 metadata branch so resolvePushSettings finds it and
	// skips fetchMetadataBranchIfMissing. Without it the test fetches the
	// resolved checkpoint URL from github.com for real — slow, flaky, and it
	// triggers the OS keychain credential helper when GitHub returns 401.
	runCheckpointRemoteGit(ctx, t, localDir, "branch", paths.MetadataBranchName)

	t.Chdir(localDir)

	ps := resolvePushSettings(ctx, "origin")

	// pushTarget() returns the checkpoint URL for checkpoint branches
	assert.Equal(t, "https://github.com/org/checkpoints.git", ps.pushTarget())
	// remote field is unchanged — trails should use this
	assert.Equal(t, "origin", ps.remote)
}

// Not parallel: uses t.Chdir()
func TestResolvePushSettings_LegacyStringConfigIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// Legacy string format should be ignored
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": "git@github.com:org/repo.git"}}`),
		0o644,
	))

	t.Chdir(tmpDir)

	ps := resolvePushSettings(t.Context(), "origin")
	assert.False(t, ps.hasCheckpointURL())
	assert.Equal(t, "origin", ps.pushTarget())
}

// Not parallel: uses t.Chdir()
func TestFetchURL_ReturnsCheckpointRemoteURL(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "git@github.com:org/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	t.Chdir(localDir)

	configured := remote.Configured(ctx)
	assert.True(t, configured)

	url, err := remote.FetchURL(ctx)
	require.NoError(t, err)
	assert.Equal(t, "git@github.com:org/checkpoints.git", url)
}

// Not parallel: uses t.Chdir()
func TestConfigured_NoCheckpointRemote(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true}`),
		0o644,
	))

	t.Chdir(localDir)

	configured := remote.Configured(t.Context())
	assert.False(t, configured)
}

// Not parallel: uses t.Chdir()
// This is the key correctness test: FetchURL must NOT apply push-side owner
// mismatch checks. A clone whose origin owner differs from the checkpoint repo
// owner should still be able to read checkpoints. That owner check is only for
// push (resolvePushSettings).
func TestFetchURL_IgnoresOwnerMismatchCheck(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Origin remote owner differs from checkpoint remote owner (alice != org).
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "git@github.com:alice/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	t.Chdir(localDir)

	configured := remote.Configured(ctx)
	assert.True(t, configured)

	// resolvePushSettings would reject this owner mismatch, but FetchURL
	// must return the URL — reading checkpoints is always allowed.
	url, err := remote.FetchURL(ctx)
	require.NoError(t, err)
	assert.Equal(t, "git@github.com:org/checkpoints.git", url)

	// Contrast: push settings should reject the same config
	ps := resolvePushSettings(ctx, "origin")
	assert.False(t, ps.hasCheckpointURL(), "resolvePushSettings should reject an origin with a different owner")
}

// Not parallel: uses t.Chdir()
func TestFetchMetadataBranch_FetchesAndCreatesLocalBranch(t *testing.T) {
	ctx := context.Background()

	// Set up a "remote" repo with entire/checkpoints/v1
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = remoteDir
	branchCmd.Env = testutil.GitIsolatedEnv()
	branchOut, err := branchCmd.Output()
	require.NoError(t, err)
	defaultBranch := strings.TrimSpace(string(branchOut))

	cmd := exec.CommandContext(ctx, "git", "checkout", "--orphan", "entire/checkpoints/v1")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "rm", "-rf", ".")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, remoteDir, "metadata.json", `{"test": true}`)
	testutil.GitAdd(t, remoteDir, "metadata.json")

	cmd = exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-m", "checkpoint data")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "checkout", defaultBranch)
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Set up local repo
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	t.Chdir(localDir)

	// Branch doesn't exist yet
	assert.False(t, testutil.BranchExists(t, localDir, "entire/checkpoints/v1"))

	// Fetch from "remote" (local path)
	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))

	// Branch should now exist
	assert.True(t, testutil.BranchExists(t, localDir, "entire/checkpoints/v1"))

	// Temp ref should be cleaned up
	assert.False(t, testutil.BranchExists(t, localDir, "refs/entire-fetch-tmp/entire/checkpoints/v1"))
}

// Not parallel: uses t.Chdir()
func TestFetchMetadataBranch_UpdatesExistingLocalBranch(t *testing.T) {
	ctx := context.Background()

	// Set up a "remote" repo with entire/checkpoints/v1
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = remoteDir
	branchCmd.Env = testutil.GitIsolatedEnv()
	branchOut, err := branchCmd.Output()
	require.NoError(t, err)
	defaultBranch := strings.TrimSpace(string(branchOut))

	cmd := exec.CommandContext(ctx, "git", "checkout", "--orphan", "entire/checkpoints/v1")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "rm", "-rf", ".")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, remoteDir, "metadata.json", `{"version": 1}`)
	testutil.GitAdd(t, remoteDir, "metadata.json")
	cmd = exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-m", "v1")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "checkout", defaultBranch)
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Set up local repo and fetch once
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))

	// Record initial hash
	hashCmd := exec.CommandContext(ctx, "git", "rev-parse", "entire/checkpoints/v1")
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	hash1Out, err := hashCmd.Output()
	require.NoError(t, err)
	hash1 := strings.TrimSpace(string(hash1Out))

	// Add a second commit on the remote
	cmd = exec.CommandContext(ctx, "git", "checkout", "entire/checkpoints/v1")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, remoteDir, "metadata.json", `{"version": 2}`)
	testutil.GitAdd(t, remoteDir, "metadata.json")
	cmd = exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-m", "v2")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "checkout", defaultBranch)
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Fetch again — should update local branch
	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))

	hashCmd = exec.CommandContext(ctx, "git", "rev-parse", "entire/checkpoints/v1")
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	hash2Out, err := hashCmd.Output()
	require.NoError(t, err)
	hash2 := strings.TrimSpace(string(hash2Out))

	assert.NotEqual(t, hash1, hash2, "FetchMetadataBranch should update existing local branch to new remote tip")
}

// TestFetchMetadataBranch_DoesNotRewindLocalAhead verifies that calling
// FetchMetadataBranch with a remote whose entire/checkpoints/v1 is at commit A
// does NOT rewind a local branch that is ahead at commit B (A's descendant).
// The buggy version unconditionally SetReferences local := tmpRef.Hash(),
// orphaning locally-committed-but-unpushed checkpoints.
//
// Not parallel: uses t.Chdir().
func TestFetchMetadataBranch_DoesNotRewindLocalAhead(t *testing.T) {
	ctx := context.Background()

	// Set up remote with metadata branch at commit A.
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = remoteDir
	branchCmd.Env = testutil.GitIsolatedEnv()
	branchOut, err := branchCmd.Output()
	require.NoError(t, err)
	defaultBranch := strings.TrimSpace(string(branchOut))

	cmd := exec.CommandContext(ctx, "git", "checkout", "--orphan", "entire/checkpoints/v1")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "rm", "-rf", ".")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, remoteDir, "metadata.json", `{"checkpoint": "A"}`)
	testutil.GitAdd(t, remoteDir, "metadata.json")
	cmd = exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-m", "checkpoint A")
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	cmd = exec.CommandContext(ctx, "git", "checkout", defaultBranch)
	cmd.Dir = remoteDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Set up local repo and fetch once so local metadata branch is at A.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	t.Chdir(localDir)

	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))

	hashCmd := exec.CommandContext(ctx, "git", "rev-parse", "entire/checkpoints/v1")
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	aOut, err := hashCmd.Output()
	require.NoError(t, err)
	aHash := strings.TrimSpace(string(aOut))

	// Advance local metadata branch to B (ahead of remote), without pushing.
	cmd = exec.CommandContext(ctx, "git", "checkout", "entire/checkpoints/v1")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, localDir, "metadata.json", `{"checkpoint": "B"}`)
	testutil.GitAdd(t, localDir, "metadata.json")
	cmd = exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-m", "checkpoint B")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	hashCmd = exec.CommandContext(ctx, "git", "rev-parse", "entire/checkpoints/v1")
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	bOut, err := hashCmd.Output()
	require.NoError(t, err)
	bHash := strings.TrimSpace(string(bOut))
	require.NotEqual(t, aHash, bHash, "test setup: local should have advanced beyond remote tip")

	// Go back to default branch — matches how the CLI runs this codepath.
	cmd = exec.CommandContext(ctx, "git", "checkout", defaultBranch)
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Fetch again — must NOT rewind local from B to A.
	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))

	hashCmd = exec.CommandContext(ctx, "git", "rev-parse", "entire/checkpoints/v1")
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	afterOut, err := hashCmd.Output()
	require.NoError(t, err)
	afterHash := strings.TrimSpace(string(afterOut))

	assert.Equal(t, bHash, afterHash,
		"FetchMetadataBranch must not rewind locally-ahead metadata branch; expected %s (B), got %s (A=%s)",
		bHash, afterHash, aHash)
}

// TestFetchMetadataBranch_DivergedPreservesLocalCheckpoint verifies that a
// metadata fetch used by read paths does not replace a diverged local branch
// with the remote tip. In the real failure mode, local has checkpoint B and
// remote has checkpoint C, both based on checkpoint A; fetching remote metadata
// must preserve B so a later push can replay it onto C.
//
// Not parallel: uses os.Chdir().
func TestFetchMetadataBranch_DivergedPreservesLocalCheckpoint(t *testing.T) {
	ctx := context.Background()

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefaultBranch := checkpointRemoteCurrentBranch(ctx, t, remoteDir)

	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "aaaaaaaaaaaa", "base")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefaultBranch)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	localDefaultBranch := checkpointRemoteCurrentBranch(ctx, t, localDir)
	t.Chdir(localDir)

	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))
	aHash := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)

	// Local advances to B without pushing.
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", paths.MetadataBranchName)
	commitCheckpointRemoteMetadata(ctx, t, localDir, "bbbbbbbbbbbb", "local-only")
	bHash := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	require.NotEqual(t, aHash, bHash, "test setup: local checkpoint branch should advance to B")
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", localDefaultBranch)

	// Remote independently advances from A to C.
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", paths.MetadataBranchName)
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "cccccccccccc", "remote-only")
	cHash := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)
	require.NotEqual(t, aHash, cHash, "test setup: remote checkpoint branch should advance to C")
	require.NotEqual(t, bHash, cHash, "test setup: local and remote tips should diverge")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefaultBranch)

	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))

	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.Contains(t, files, "aa/aaaaaaaaaa/metadata.json", "base checkpoint should be preserved")
	assert.Contains(t, files, "cc/cccccccccc/metadata.json", "remote checkpoint should be present after fetch")
	assert.Contains(t, files, "bb/bbbbbbbbbb/metadata.json", "local-only checkpoint should be preserved after diverged metadata fetch")

	afterHash := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	assert.Equal(t, cHash, checkpointRemoteRevParse(ctx, t, localDir, afterHash+"^"),
		"diverged fetch promotion should replay local commits directly onto the fetched remote tip")
}

// TestFetchMetadataBranch_DisconnectedPreservesLocalCheckpoint verifies the
// safety fallback when the local and fetched checkpoint branches share no
// ancestry. There is no previous base to compute, so all local checkpoint
// commits are replayed onto the fetched tip instead of replacing local state.
//
// Not parallel: uses os.Chdir().
func TestFetchMetadataBranch_DisconnectedPreservesLocalCheckpoint(t *testing.T) {
	ctx := context.Background()

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefaultBranch := checkpointRemoteCurrentBranch(ctx, t, remoteDir)

	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "aaaaaaaaaaaa", "old-base")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefaultBranch)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	localDefaultBranch := checkpointRemoteCurrentBranch(ctx, t, localDir)
	t.Chdir(localDir)

	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", paths.MetadataBranchName)
	commitCheckpointRemoteMetadata(ctx, t, localDir, "bbbbbbbbbbbb", "local-only")
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", localDefaultBranch)

	// Replace the remote checkpoint branch with an unrelated orphan history.
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", "replacement-checkpoints")
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "cccccccccccc", "remote-rewrite")
	runCheckpointRemoteGit(ctx, t, remoteDir, "branch", "-M", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefaultBranch)

	require.NoError(t, FetchMetadataBranch(ctx, remoteDir))

	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.Contains(t, files, "cc/cccccccccc/metadata.json", "rewritten remote checkpoint should be present after fetch")
	assert.Contains(t, files, "bb/bbbbbbbbbb/metadata.json", "local-only checkpoint should be replayed when there is no common ancestor")
}

// TestEnsurePrimaryRef_FetchesFromCheckpointRemoteInsteadOfOrphan reproduces
// issue #1374: enabling Entire on a second device where a checkpoint_remote is
// configured and already holds entire/checkpoints/v1 must fetch that branch
// rather than creating an empty orphan (which hides existing checkpoints and is
// later rejected non-fast-forward).
//
// Not parallel: uses t.Chdir().
func TestEnsurePrimaryRef_FetchesFromCheckpointRemoteInsteadOfOrphan(t *testing.T) {
	ctx := context.Background()

	// Checkpoint remote: a repo that already holds entire/checkpoints/v1 with a
	// real (non-empty) commit — models the branch created on device A.
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefaultBranch := checkpointRemoteCurrentBranch(ctx, t, remoteDir)

	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "aaaaaaaaaaaa", "device-a")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefaultBranch)
	remoteTip := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)

	// Local repo (device B): origin points at the main repo and a separate
	// checkpoint_remote is configured; the local metadata branch does not exist.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// The SSH origin + github checkpoint_remote resolves (via remote.FetchURL)
	// to git@github.com:org/checkpoints.git. Redirect that derived URL to the
	// local file:// remote so the real fetch path runs hermetically.
	redirectGitURL(t, localDir, "git@github.com:org/checkpoints.git", "file://"+remoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	// Sanity: derivation produces the URL we redirected.
	url, err := remote.FetchURL(ctx)
	require.NoError(t, err)
	require.Equal(t, "git@github.com:org/checkpoints.git", url)

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	// Only an explicit setup flow (WithCheckpointRemoteBootstrap) is allowed
	// to fetch from the checkpoint remote here — see
	// TestEnsurePrimaryRef_SkipsCheckpointRemoteBootstrapOutsideEnableFlow for
	// the per-turn hot-path behavior.
	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo))

	// The local metadata branch must now match the checkpoint remote's tip,
	// not a fresh empty orphan.
	localTip := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	assert.Equal(t, remoteTip, localTip,
		"EnsurePrimaryRef must fetch entire/checkpoints/v1 from the configured checkpoint_remote instead of creating an empty orphan")

	// And it must carry the real checkpoint data (proving it is not an empty tree).
	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.Contains(t, files, "aa/aaaaaaaaaa/"+paths.MetadataFileName,
		"the bootstrapped branch should contain the checkpoint committed on the remote")
}

// TestEnsurePrimaryRef_ReplacesExistingEmptyOrphanFromCheckpointRemote verifies
// that a *pre-existing* local empty orphan (the exact shape a pre-#1374
// `entire enable` left behind) is still healed on a later run of an explicit
// setup flow, not just the "local ref missing entirely" case. Origin does not
// track Primary here (checkpoint_remote strategy), so remoteRef is nil and the
// only recovery path is fetching from the configured checkpoint_remote.
func TestEnsurePrimaryRef_ReplacesExistingEmptyOrphanFromCheckpointRemote(t *testing.T) {
	ctx := context.Background()

	// Checkpoint remote: a repo that already holds entire/checkpoints/v1 with a
	// real (non-empty) commit — models the branch created on device A.
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefaultBranch := checkpointRemoteCurrentBranch(ctx, t, remoteDir)

	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "aaaaaaaaaaaa", "device-a")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefaultBranch)
	remoteTip := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)

	// Local repo (device B): origin points at the main repo and a separate
	// checkpoint_remote is configured. Unlike the "missing ref" test above, this
	// device already has a local empty orphan on entire/checkpoints/v1 — the
	// state left behind by the pre-#1374 `entire enable` before this fix existed.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, localDir, "rm", "-rf", ".")
	runCheckpointRemoteGit(ctx, t, localDir, "commit", "--allow-empty", "-m", "Initialize metadata ref")
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", remoteDefaultBranch)

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// The SSH origin + github checkpoint_remote resolves (via remote.FetchURL)
	// to git@github.com:org/checkpoints.git. Redirect that derived URL to the
	// local file:// remote so the real fetch path runs hermetically.
	redirectGitURL(t, localDir, "git@github.com:org/checkpoints.git", "file://"+remoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	// Re-running `entire enable` (explicit setup flow) after upgrading past
	// #1374 must still recover the real branch, even though a local ref already
	// exists — it must not return early just because localRef was found.
	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo))

	localTip := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	assert.Equal(t, remoteTip, localTip,
		"EnsurePrimaryRef must heal a pre-existing empty orphan by fetching from checkpoint_remote")

	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.Contains(t, files, "aa/aaaaaaaaaa/"+paths.MetadataFileName,
		"the healed branch should contain the checkpoint committed on the remote")
}

// TestEnsurePrimaryRef_SkipsCheckpointRemoteBootstrapOutsideEnableFlow verifies
// that EnsurePrimaryRef never fetches from a configured checkpoint_remote
// unless the caller explicitly opts in via WithCheckpointRemoteBootstrap. This
// models the per-turn hook hot path (EnsureSetup runs synchronously on every
// TurnStart hook, which has a hard execution timeout): steady-state must stay
// network-free even when a checkpoint_remote with real data is configured.
func TestEnsurePrimaryRef_SkipsCheckpointRemoteBootstrapOutsideEnableFlow(t *testing.T) {
	ctx := context.Background()

	// Checkpoint remote: a repo that already holds entire/checkpoints/v1 with a
	// real (non-empty) commit — models the branch created on device A.
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefaultBranch := checkpointRemoteCurrentBranch(ctx, t, remoteDir)

	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "aaaaaaaaaaaa", "device-a")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefaultBranch)

	// Local repo: origin points at the main repo and a separate
	// checkpoint_remote is configured; the local metadata branch does not exist.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// Redirect the derived checkpoint_remote URL to the local file:// remote.
	// If EnsurePrimaryRef were to fetch here, this hermetic redirect would let
	// it succeed — so a passing test proves the fetch was skipped, not merely
	// that it failed silently.
	redirectGitURL(t, localDir, "git@github.com:org/checkpoints.git", "file://"+remoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	// No WithCheckpointRemoteBootstrap here — steady-state per-turn path.
	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	// The local metadata branch must be a fresh empty orphan, not the fetched
	// checkpoint remote data: the network fetch must never have happened.
	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.NotContains(t, files, "aa/aaaaaaaaaa/"+paths.MetadataFileName,
		"EnsurePrimaryRef must not fetch from checkpoint_remote outside an explicit enable flow")
}

// TestEnsurePrimaryRef_OfflineCheckpointRemoteFallsBackToOrphan verifies that
// even in an explicit setup flow (WithCheckpointRemoteBootstrap), an
// unreachable checkpoint_remote does not fail EnsurePrimaryRef or hang the
// caller — it must fall back to creating the empty orphan, matching the
// pre-existing offline/no-remote behavior.
func TestEnsurePrimaryRef_OfflineCheckpointRemoteFallsBackToOrphan(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// Redirect the derived checkpoint_remote URL to a nonexistent local path
	// so the fetch fails fast (no network access, no hang) rather than
	// exercising the real 30s timeout.
	redirectGitURL(t, localDir, "git@github.com:org/checkpoints.git", "file:///nonexistent/checkpoints.git")

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo),
		"EnsurePrimaryRef must not fail when the checkpoint remote is unreachable")

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "empty orphan fallback must still be created")
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	assert.Empty(t, tree.Entries, "expected empty orphan fallback when checkpoint remote is unreachable")
}

// TestURLTargetsCheckpointRepo verifies the issue #1374 guard that distinguishes a
// derived checkpoint URL from remote.FetchURL's silent origin fallback: only URLs
// whose owner/repo match the configured checkpoint repo (host-agnostic,
// case-insensitive) are accepted.
func TestURLTargetsCheckpointRepo(t *testing.T) {
	t.Parallel()

	config := &settings.CheckpointRemoteConfig{Provider: "github", Repo: "org/checkpoints"}

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"HTTPS checkpoint repo", "https://github.com/org/checkpoints.git", true},
		{"SSH checkpoint repo", "git@github.com:org/checkpoints.git", true},
		{"enterprise host, same repo path", "https://github.example.com/org/checkpoints.git", true},
		{"case-insensitive owner/repo", "https://github.com/Org/Checkpoints.git", true},
		{"origin fallback (different repo)", "https://github.com/org/main-repo.git", false},
		{"different owner", "https://github.com/other/checkpoints.git", false},
		{"unparseable url", "not a url", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, urlTargetsCheckpointRepo(tt.url, config))
		})
	}
}

// TestEnsurePrimaryRef_CheckpointRemoteTakesPrecedenceOverOrigin verifies issue
// #1374: when a checkpoint_remote is configured, EnsurePrimaryRef adopts its branch
// even when a stale origin/entire/checkpoints/v1 tracking ref is present. Origin is
// no longer the authoritative checkpoint store, so it must not be seeded from.
//
// Not parallel: uses t.Chdir().
func TestEnsurePrimaryRef_CheckpointRemoteTakesPrecedenceOverOrigin(t *testing.T) {
	ctx := context.Background()

	// Checkpoint remote holds the authoritative checkpoint.
	checkpointRemoteDir := t.TempDir()
	testutil.InitRepo(t, checkpointRemoteDir)
	testutil.WriteFile(t, checkpointRemoteDir, "f.txt", "init")
	testutil.GitAdd(t, checkpointRemoteDir, "f.txt")
	testutil.GitCommit(t, checkpointRemoteDir, "init")
	cpDefault := checkpointRemoteCurrentBranch(ctx, t, checkpointRemoteDir)
	runCheckpointRemoteGit(ctx, t, checkpointRemoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, checkpointRemoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, checkpointRemoteDir, "aaaaaaaaaaaa", "authoritative")
	runCheckpointRemoteGit(ctx, t, checkpointRemoteDir, "checkout", cpDefault)
	cpTip := checkpointRemoteRevParse(ctx, t, checkpointRemoteDir, paths.MetadataBranchName)

	// Origin holds a different, stale checkpoint branch.
	originDir := t.TempDir()
	testutil.InitRepo(t, originDir)
	testutil.WriteFile(t, originDir, "f.txt", "init")
	testutil.GitAdd(t, originDir, "f.txt")
	testutil.GitCommit(t, originDir, "init")
	originDefault := checkpointRemoteCurrentBranch(ctx, t, originDir)
	runCheckpointRemoteGit(ctx, t, originDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, originDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, originDir, "bbbbbbbbbbbb", "stale")
	runCheckpointRemoteGit(ctx, t, originDir, "checkout", originDefault)

	// Local repo (device B): fetch origin so a stale origin tracking ref exists,
	// then repoint origin at an SSH URL so FetchURL derives the github checkpoint
	// URL. The stale tracking ref and its objects survive the set-url.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", "file://"+originDir)
	runCheckpointRemoteGit(ctx, t, localDir, "fetch", "origin")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "set-url", "origin", "git@github.com:org/main-repo.git")

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, paths.SettingsFileName),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	// The SSH origin + github checkpoint_remote resolves (via remote.FetchURL) to
	// git@github.com:org/checkpoints.git. Redirect that derived URL to the local
	// checkpoint remote so the real fetch path runs hermetically.
	redirectGitURL(t, localDir, "git@github.com:org/checkpoints.git", "file://"+checkpointRemoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	// Sanity: the stale origin tracking ref exists and differs from the remote.
	originRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), true)
	require.NoError(t, err, "test setup: origin tracking ref should exist")
	require.NotEqual(t, cpTip, originRef.Hash().String(), "test setup: origin must differ from checkpoint remote")

	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo))

	got := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	assert.Equal(t, cpTip, got, "local branch should adopt the checkpoint remote, not the stale origin ref")
	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.Contains(t, files, "aa/aaaaaaaaaa/"+paths.MetadataFileName, "authoritative checkpoint-remote data should be present")
	assert.NotContains(t, files, "bb/bbbbbbbbbb/"+paths.MetadataFileName, "stale origin data must not be adopted")
}

// TestEnsurePrimaryRef_HealsVercelOnlyOrphanFromCheckpointRemote verifies the issue
// #1374 heal covers a local metadata branch carrying only vercel.json — the
// orphan-init state in a vercel-enabled repo. A literal empty-tree check skipped it
// (the tree is not empty), leaving those devices divergent forever.
//
// Not parallel: uses t.Chdir().
func TestEnsurePrimaryRef_HealsVercelOnlyOrphanFromCheckpointRemote(t *testing.T) {
	ctx := context.Background()

	// Checkpoint remote holds the authoritative checkpoint.
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefault := checkpointRemoteCurrentBranch(ctx, t, remoteDir)
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "aaaaaaaaaaaa", "device-a")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefault)
	remoteTip := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)

	// Local repo (device B): a local orphan carrying only vercel.json — the
	// vercel-enabled bug state left behind by orphan initialization.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")
	localDefault := checkpointRemoteCurrentBranch(ctx, t, localDir)
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, localDir, "rm", "-rf", ".")
	testutil.WriteFile(t, localDir, vercelconfig.FileName, `{"git":{"deploymentEnabled":{"entire/**":false}}}`)
	runCheckpointRemoteGit(ctx, t, localDir, "add", vercelconfig.FileName)
	runCheckpointRemoteGit(ctx, t, localDir, "commit", "-m", "Initialize metadata branch")
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", localDefault)

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, paths.SettingsFileName),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))
	redirectGitURL(t, localDir, "git@github.com:org/checkpoints.git", "file://"+remoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	vercelOnlyTip := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	require.NotEqual(t, remoteTip, vercelOnlyTip, "test setup: vercel-only orphan must differ from remote tip")

	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo))

	healed := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	assert.Equal(t, remoteTip, healed,
		"a vercel.json-only orphan should be treated as un-initialized and healed to the exact remote tip")
	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.Contains(t, files, "aa/aaaaaaaaaa/"+paths.MetadataFileName, "the healed branch should contain the checkpoint remote data")
}

// redirectGitURL appends a git `url.<replacement>.insteadOf = <match>` rule to
// the repo-local config so any git operation on matchURL is transparently
// rewritten to replacementURL. This lets tests point a derived remote URL at a
// local file:// repository with no network access. Repo-local config is honored
// regardless of the ambient GIT_CONFIG_* environment.
func redirectGitURL(t *testing.T, repoDir, matchURL, replacementURL string) { //nolint:unparam // matchURL happens to be the same derived checkpoint_remote URL across current callers; kept parameterized for test clarity and future callers with a different remote shape
	t.Helper()
	configPath := filepath.Join(repoDir, ".git", "config")
	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	_, err = fmt.Fprintf(f, "\n[url %q]\n\tinsteadOf = %s\n", replacementURL, matchURL)
	require.NoError(t, err)
}

func runCheckpointRemoteGit(ctx context.Context, t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
}

func checkpointRemoteCurrentBranch(ctx context.Context, t *testing.T, dir string) string {
	t.Helper()
	return checkpointRemoteGitOutput(ctx, t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func checkpointRemoteRevParse(ctx context.Context, t *testing.T, dir, rev string) string {
	t.Helper()
	return checkpointRemoteGitOutput(ctx, t, dir, "rev-parse", rev)
}

func commitCheckpointRemoteMetadata(ctx context.Context, t *testing.T, dir, checkpointID, label string) {
	t.Helper()
	checkpointDir := filepath.Join(dir, checkpointID[:2], checkpointID[2:])
	require.NoError(t, os.MkdirAll(checkpointDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(checkpointDir, paths.MetadataFileName),
		[]byte(fmt.Sprintf(`{"checkpoint_id":%q}`, checkpointID)),
		0o644,
	))
	runCheckpointRemoteGit(ctx, t, dir, "add", ".")
	runCheckpointRemoteGit(ctx, t, dir, "commit", "-m", "Checkpoint: "+checkpointID+" "+label)
}

func checkpointRemoteMetadataFiles(ctx context.Context, t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-r", "--name-only", "refs/heads/"+paths.MetadataBranchName)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return string(out)
}

// checkpointRemoteWithV1 builds a checkpoint remote holding entire/checkpoints/v1
// with one real checkpoint, and returns its directory. Used by the git-refs
// bootstrap-skip tests, where the remote must be genuinely fetchable so a
// passing assertion proves the fetch was skipped rather than that it failed.
func checkpointRemoteWithV1(ctx context.Context, t *testing.T) string {
	t.Helper()
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	defaultBranch := checkpointRemoteCurrentBranch(ctx, t, remoteDir)

	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "aaaaaaaaaaaa", "device-a")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", defaultBranch)
	return remoteDir
}

// gitRefsPrimaryLocalRepo builds a local repo configured with a github
// checkpoint_remote and the git-refs primary backend, with the derived
// checkpoint URL redirected at remoteDir so any fetch would succeed.
func checkpointRemoteLocalRepo(ctx context.Context, t *testing.T, remoteDir, settingsJSON string) string {
	t.Helper()
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", "git@github.com:org/main-repo.git")

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsJSON), 0o644))

	// A hermetic redirect that WOULD serve the branch: a test asserting the
	// branch is absent therefore proves the fetch never ran, not that it ran
	// and failed.
	redirectGitURL(t, localDir, "git@github.com:org/checkpoints.git", "file://"+remoteDir)
	return localDir
}

// gitRefsPrimaryLocalRepo is checkpointRemoteLocalRepo with the git-refs primary
// selected in settings.
func gitRefsPrimaryLocalRepo(ctx context.Context, t *testing.T, remoteDir string) string {
	t.Helper()
	return checkpointRemoteLocalRepo(ctx, t, remoteDir,
		`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}, "checkpoints": {"primary": {"type": "git-refs"}}}`)
}

// TestEnsurePrimaryRef_SkipsCheckpointRemoteBootstrapUnderGitRefsPrimary pins that
// `entire enable` does not eagerly clone the v1 metadata branch when the git-refs
// backend is primary. That backend never writes v1, so there is no local orphan
// that could diverge from the remote — the only thing the bootstrap exists to
// prevent — while v1 carries the full legacy transcript history, making the fetch
// arbitrarily expensive on a real checkpoint remote.
//
// Note this is an explicit enable flow (WithCheckpointRemoteBootstrap), unlike
// TestEnsurePrimaryRef_SkipsCheckpointRemoteBootstrapOutsideEnableFlow: the
// backend, not the calling context, is what suppresses the fetch here.
func TestEnsurePrimaryRef_SkipsCheckpointRemoteBootstrapUnderGitRefsPrimary(t *testing.T) {
	ctx := context.Background()

	remoteDir := checkpointRemoteWithV1(ctx, t)
	localDir := gitRefsPrimaryLocalRepo(ctx, t, remoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo))

	// A bootstrap fetch would have created the local v1 ref at the remote's tip.
	// Under git-refs no v1 ref should exist at all: not fetched, and not seeded
	// as an empty orphan either.
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"EnsurePrimaryRef must not fetch or seed v1 under the git-refs primary backend")
}

// TestResolvePushSettings_SkipsMetadataFetchUnderGitRefsPrimary pins that the
// pre-push path does not fetch the v1 metadata branch under the git-refs primary
// backend. fetchMetadataBranchIfMissing is written as a one-time cost that stops
// once the branch exists locally, but git-refs pushes per-checkpoint refs and
// never creates v1 locally — so without this gate the "one-time" fetch would run
// on every single `git push`.
func TestResolvePushSettings_SkipsMetadataFetchUnderGitRefsPrimary(t *testing.T) {
	ctx := context.Background()

	remoteDir := checkpointRemoteWithV1(ctx, t)
	localDir := gitRefsPrimaryLocalRepo(ctx, t, remoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	ps := resolvePushSettings(ctx, "origin")

	// The checkpoint URL still resolves — only the eager fetch is suppressed.
	assert.True(t, ps.hasCheckpointURL(), "checkpoint_remote should still resolve under git-refs")
	assert.Equal(t, "git@github.com:org/checkpoints.git", ps.pushTarget())

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"resolvePushSettings must not fetch v1 under the git-refs primary backend")
}

// checkpointRemoteGitOutput runs a git command in dir and returns trimmed stdout.
func checkpointRemoteGitOutput(ctx context.Context, t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// TestEnsurePrimaryRef_BootstrapLandsReadableBranch pins that after the
// enable-time bootstrap, the branch it landed is actually readable — not just
// present as a ref.
//
// A blob-filtered bootstrap looks safe (the bootstrap itself only needs the ref
// to stop a later orphan diverging) and is not. The bootstrap is only ever
// reached under the git-branch primary, where the branch it lands IS the repo's
// checkpoint store: GitStore.List reads each checkpoint's metadata.json through
// a plain tree with no blob fetcher, and the read path's recovery tier is keyed
// on the ref being missing — so once the filtered fetch lands the ref, nothing
// ever backfills the blobs and `checkpoint list` shows bare IDs forever.
//
// Asserting on the blob rather than on fetch flags is deliberate: it pins the
// property that matters (the store is readable) rather than the mechanism.
func TestEnsurePrimaryRef_BootstrapLandsReadableBranch(t *testing.T) {
	ctx := context.Background()

	remoteDir := checkpointRemoteWithV1(ctx, t)
	// --filter is only honored over the smart protocol with allowFilter set
	// server-side; the redirect below already makes this a file:// URL.
	runCheckpointRemoteGit(ctx, t, remoteDir, "config", "uploadpack.allowFilter", "true")

	remoteTip := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)
	metadataBlob := checkpointRemoteRevParse(ctx, t, remoteDir,
		"refs/heads/"+paths.MetadataBranchName+":aa/aaaaaaaaaa/"+paths.MetadataFileName)

	// git-branch primary (no checkpoints block) so the bootstrap actually runs.
	localDir := checkpointRemoteLocalRepo(ctx, t, remoteDir,
		`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}, "filtered_fetches": true}}`)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo))

	// The bootstrap still did its job: the local ref tracks the remote tip, so
	// no divergent orphan can be created on top of it.
	assert.Equal(t, remoteTip, checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName),
		"bootstrap must still land the local ref at the checkpoint remote's tip")

	// ...and the checkpoint's metadata blob came with it. rev-list --missing=print
	// reports absent objects with a "?" prefix without triggering the lazy fetch
	// that `git cat-file` would, so this distinguishes "present" from "promised".
	missing := checkpointRemoteGitOutput(ctx, t, localDir,
		"rev-list", "--objects", "--missing=print", "refs/heads/"+paths.MetadataBranchName)
	assert.NotContains(t, missing, "?"+metadataBlob,
		"bootstrap must land a readable branch: without metadata.json, checkpoint list shows a bare ID with no prompt, date, or counts")
}

// TestFetchBranchIfMissing_ReportsUnreachableRemote pins that a checkpoint remote
// we could not talk to is reported to the caller rather than swallowed. It used
// to return nil on every fetch failure, which made an unreachable, timing-out, or
// auth-refusing remote indistinguishable from one that was never contacted — so
// resolvePushSettings' warning could never fire and the failure left no trace.
//
// Contrast TestFetchBranchIfMissing_NoOpWhenBranchNotOnRemote: a reachable remote
// that simply lacks the branch is still a quiet no-op.
func TestFetchBranchIfMissing_ReportsUnreachableRemote(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	t.Chdir(localDir)

	// A path that is not a git repository at all: git fails to connect rather
	// than reporting the ref as absent.
	err := fetchMetadataBranchIfMissing(ctx, filepath.Join(t.TempDir(), "nonexistent"))
	require.Error(t, err, "an unreachable checkpoint remote must surface to the caller")

	// The branch must still not exist locally — reporting the failure does not
	// change the fail-soft outcome, only its visibility.
	assert.False(t, testutil.BranchExists(t, localDir, paths.MetadataBranchName))
}

// TestEnsurePrimaryRef_SkipsEmptyOrphanHealUnderGitRefsPrimary pins the third
// checkpoint-remote fetch path against the same rule as the other two.
//
// healPrimaryFromCheckpointRemote runs whenever a local v1 ref already exists and
// carries no data — the shape a repo enabled before the git-refs switch is left
// in. It was the one fetch path not gated on the backend, so `entire enable` on
// such a repo still pulled the whole transcript archive, on the raised foreground
// budget, for a branch git-refs never writes.
//
// Reads are unaffected: a data-free branch is treated as a miss by the read path,
// which recovers it on demand (checkpoint.TestGetSessionsBranchTree_RecoversFromDataFreeOrphan).
func TestEnsurePrimaryRef_SkipsEmptyOrphanHealUnderGitRefsPrimary(t *testing.T) {
	ctx := context.Background()

	remoteDir := checkpointRemoteWithV1(ctx, t)
	localDir := gitRefsPrimaryLocalRepo(ctx, t, remoteDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	// Stand up the data-free orphan the heal exists to replace.
	workBranch := checkpointRemoteCurrentBranch(ctx, t, localDir)
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, localDir, "rm", "-rf", ".")
	runCheckpointRemoteGit(ctx, t, localDir, "commit", "--allow-empty", "-m", "init orphan")
	orphanTip := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", workBranch)

	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(WithCheckpointRemoteBootstrap(ctx), repo))

	// The redirect would have served the branch, so an unchanged tip proves the
	// heal fetch was skipped rather than attempted and failed.
	assert.Equal(t, orphanTip, checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName),
		"enable must not heal the v1 orphan from the checkpoint remote under git-refs primary")
}
