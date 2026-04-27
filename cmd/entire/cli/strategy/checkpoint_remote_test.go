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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveCheckpointURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		pushRemoteURL  string
		checkpointRepo string
		want           string
		wantErr        bool
	}{
		{
			name:           "SSH push remote",
			pushRemoteURL:  "git@github.com:org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "git@github.com:org/checkpoints.git",
		},
		{
			name:           "HTTPS push remote",
			pushRemoteURL:  "https://github.com/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "https://github.com/org/checkpoints.git",
		},
		{
			name:           "SSH protocol push remote",
			pushRemoteURL:  "ssh://git@github.com/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "git@github.com:org/checkpoints.git",
		},
		{
			name:           "different host",
			pushRemoteURL:  "git@github.example.com:org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "git@github.example.com:org/checkpoints.git",
		},
		{
			name:           "HTTPS with non-standard port",
			pushRemoteURL:  "https://git.example.com:8443/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "https://git.example.com:8443/org/checkpoints.git",
		},
		{
			name:           "SSH protocol with non-standard port",
			pushRemoteURL:  "ssh://git@git.example.com:2222/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "ssh://git@git.example.com:2222/org/checkpoints.git",
		},
		{
			name:           "invalid push remote",
			pushRemoteURL:  "not-a-url",
			checkpointRepo: "org/checkpoints",
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := &settings.CheckpointRemoteConfig{Provider: "github", Repo: tt.checkpointRepo}
			got, err := remote.DeriveCheckpointURL(tt.pushRemoteURL, config)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

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

// v2RefSeq is a counter to ensure each call to createV2MainRef produces a distinct commit.
var v2RefSeq int

// createV2MainRef creates a v2 /main custom ref with a single orphan commit.
// Uses git plumbing to create the ref under refs/entire/ (not refs/heads/).
// Each call produces a distinct commit (uses a sequence counter in content).
func createV2MainRef(ctx context.Context, t *testing.T, repoDir string) {
	t.Helper()
	v2RefSeq++

	cmd := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`{"test": true, "seq": %d}`, v2RefSeq))
	blobOut, err := cmd.Output()
	require.NoError(t, err)
	blobHash := strings.TrimSpace(string(blobOut))

	cmd = exec.CommandContext(ctx, "git", "mktree")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	cmd.Stdin = strings.NewReader("100644 blob " + blobHash + "\tmetadata.json\n")
	treeOut, err := cmd.Output()
	require.NoError(t, err)
	treeHash := strings.TrimSpace(string(treeOut))

	cmd = exec.CommandContext(ctx, "git", "commit-tree", "-m", fmt.Sprintf("v2 checkpoint %d", v2RefSeq), treeHash)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	commitOut, err := cmd.Output()
	require.NoError(t, err)
	commitHash := strings.TrimSpace(string(commitOut))

	cmd = exec.CommandContext(ctx, "git", "update-ref", paths.V2MainRefName, commitHash)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}

// refExists checks whether a custom ref exists in the repo.
func refExists(ctx context.Context, t *testing.T, repoDir, refName string) bool {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", refName)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}

// Not parallel: uses t.Chdir()
func TestFetchV2MainFromURL_FetchesRef(t *testing.T) {
	ctx := context.Background()

	// Set up "remote" repo with v2 /main ref
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	createV2MainRef(ctx, t, remoteDir)

	// Set up local repo
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	t.Chdir(localDir)

	// Ref doesn't exist yet
	assert.False(t, refExists(ctx, t, localDir, paths.V2MainRefName))

	// Fetch from "remote"
	require.NoError(t, FetchV2MainFromURL(ctx, remoteDir))

	// Ref should now exist
	assert.True(t, refExists(ctx, t, localDir, paths.V2MainRefName))
}

// Not parallel: uses t.Chdir()
func TestFetchV2MainFromURL_UpdatesExistingRef(t *testing.T) {
	ctx := context.Background()

	// Set up "remote" repo with v2 /main ref
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	createV2MainRef(ctx, t, remoteDir)

	// Set up local repo and fetch once
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	t.Chdir(localDir)

	require.NoError(t, FetchV2MainFromURL(ctx, remoteDir))

	// Record initial hash
	hashCmd := exec.CommandContext(ctx, "git", "rev-parse", paths.V2MainRefName)
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	hash1Out, err := hashCmd.Output()
	require.NoError(t, err)
	hash1 := strings.TrimSpace(string(hash1Out))

	// Add a second commit on the remote's v2 ref
	createV2MainRef(ctx, t, remoteDir) // Creates a new orphan commit, updating the ref

	// Fetch again — should update
	require.NoError(t, FetchV2MainFromURL(ctx, remoteDir))

	hashCmd = exec.CommandContext(ctx, "git", "rev-parse", paths.V2MainRefName)
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	hash2Out, err := hashCmd.Output()
	require.NoError(t, err)
	hash2 := strings.TrimSpace(string(hash2Out))

	assert.NotEqual(t, hash1, hash2, "FetchV2MainFromURL should update existing ref to new remote tip")
}

// TestFetchV2MainFromURL_DoesNotRewindLocalAhead verifies that fetching the v2
// /main ref from a remote whose tip is at A does NOT rewind a locally-ahead
// ref at B (A's descendant). The buggy version used a direct-write refspec
// `+refs/entire/v2/main:refs/entire/v2/main` which git applies before Go can
// intercept — orphaning locally-committed-but-unpushed v2 checkpoints.
//
// Not parallel: uses t.Chdir().
func TestFetchV2MainFromURL_DoesNotRewindLocalAhead(t *testing.T) {
	ctx := context.Background()

	// Set up remote with v2 /main ref at commit A.
	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	createV2MainRef(ctx, t, remoteDir)

	// Set up local repo and fetch once so local v2 /main is at A.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	t.Chdir(localDir)

	require.NoError(t, FetchV2MainFromURL(ctx, remoteDir))

	hashCmd := exec.CommandContext(ctx, "git", "rev-parse", paths.V2MainRefName)
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	aOut, err := hashCmd.Output()
	require.NoError(t, err)
	aHash := strings.TrimSpace(string(aOut))

	// Advance local v2 /main to B, with A as parent (descendant relationship).
	// This mirrors the real flow where condensation appends a new checkpoint
	// commit on top of the previous tip.
	advanceV2MainOnTop(ctx, t, localDir, aHash)

	hashCmd = exec.CommandContext(ctx, "git", "rev-parse", paths.V2MainRefName)
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	bOut, err := hashCmd.Output()
	require.NoError(t, err)
	bHash := strings.TrimSpace(string(bOut))
	require.NotEqual(t, aHash, bHash, "test setup: local v2 /main should have advanced beyond remote")

	// Fetch from remote — must NOT rewind local from B to A.
	require.NoError(t, FetchV2MainFromURL(ctx, remoteDir))

	hashCmd = exec.CommandContext(ctx, "git", "rev-parse", paths.V2MainRefName)
	hashCmd.Dir = localDir
	hashCmd.Env = testutil.GitIsolatedEnv()
	afterOut, err := hashCmd.Output()
	require.NoError(t, err)
	afterHash := strings.TrimSpace(string(afterOut))

	assert.Equal(t, bHash, afterHash,
		"FetchV2MainFromURL must not rewind locally-ahead v2 /main; expected %s (B), got %s (A=%s)",
		bHash, afterHash, aHash)
}

// advanceV2MainOnTop creates a new v2 /main commit whose parent is parentHash,
// and updates refs/entire/checkpoints/v2/main to point at it. Used to simulate
// a locally-ahead ref in tests.
func advanceV2MainOnTop(ctx context.Context, t *testing.T, repoDir, parentHash string) {
	t.Helper()
	v2RefSeq++

	cmd := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`{"advance": %d}`, v2RefSeq))
	blobOut, err := cmd.Output()
	require.NoError(t, err)
	blobHash := strings.TrimSpace(string(blobOut))

	cmd = exec.CommandContext(ctx, "git", "mktree")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	cmd.Stdin = strings.NewReader("100644 blob " + blobHash + "\tadvance.json\n")
	treeOut, err := cmd.Output()
	require.NoError(t, err)
	treeHash := strings.TrimSpace(string(treeOut))

	cmd = exec.CommandContext(ctx, "git", "commit-tree", "-p", parentHash, "-m", fmt.Sprintf("advance %d", v2RefSeq), treeHash)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	commitOut, err := cmd.Output()
	require.NoError(t, err)
	commitHash := strings.TrimSpace(string(commitOut))

	cmd = exec.CommandContext(ctx, "git", "update-ref", paths.V2MainRefName, commitHash)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}
