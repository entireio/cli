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

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
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
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, ".entire", paths.SettingsFileName),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_version": "1.1"}}`),
		0o644,
	))
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
	assert.Equal(t, hash2, checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataRefName),
		"FetchMetadataBranch should mirror fetched v1 metadata to the v1.1 custom ref")
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

// TestBootstrapMetadataFromURL_FetchesWhenLocalMissing verifies the issue #1374
// fix: when no local metadata branch exists, the branch is fetched from the
// checkpoint remote and points exactly at the remote tip — no empty orphan, no
// replayed commit on top.
//
// Not parallel: uses t.Chdir().
func TestBootstrapMetadataFromURL_FetchesWhenLocalMissing(t *testing.T) {
	ctx := context.Background()

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefault := checkpointRemoteCurrentBranch(ctx, t, remoteDir)
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "abcabcabcabc", "data")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefault)
	remoteTip := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	t.Chdir(localDir)

	repo, err := git.PlainOpen(localDir)
	require.NoError(t, err)

	require.False(t, testutil.BranchExists(t, localDir, paths.MetadataBranchName),
		"test setup: local metadata branch should not exist yet")

	ok, err := bootstrapMetadataFromURL(ctx, repo, remoteDir)
	require.NoError(t, err)
	assert.True(t, ok, "bootstrap should report success when the remote has real metadata")
	assert.True(t, testutil.BranchExists(t, localDir, paths.MetadataBranchName))
	assert.Equal(t, remoteTip, checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName),
		"local branch should point exactly at the remote tip, not at a fresh orphan")
}

// TestBootstrapMetadataFromURL_EmptyRemoteReturnsFalse verifies that fetching a
// remote whose metadata branch is itself an empty orphan does not count as a
// successful bootstrap, so the caller falls back to its normal orphan creation.
//
// Not parallel: uses t.Chdir().
func TestBootstrapMetadataFromURL_EmptyRemoteReturnsFalse(t *testing.T) {
	ctx := context.Background()

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	createEmptyOrphanMetadataBranch(ctx, t, remoteDir)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	t.Chdir(localDir)

	repo, err := git.PlainOpen(localDir)
	require.NoError(t, err)

	ok, err := bootstrapMetadataFromURL(ctx, repo, remoteDir)
	require.NoError(t, err)
	assert.False(t, ok, "an empty remote metadata branch should not be treated as a successful bootstrap")
}

// TestHealEmptyOrphanFromURL_ReplacesEmptyOrphanWithRemoteData verifies that an
// existing empty-orphan metadata branch (the issue #1374 bug state) is replaced
// wholesale with the real branch from the checkpoint remote — the local ref ends
// up at the exact remote tip with no replayed empty commit on top.
//
// Not parallel: uses t.Chdir().
func TestHealEmptyOrphanFromURL_ReplacesEmptyOrphanWithRemoteData(t *testing.T) {
	ctx := context.Background()

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefault := checkpointRemoteCurrentBranch(ctx, t, remoteDir)
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "abcabcabcabc", "data")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefault)
	remoteTip := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	createEmptyOrphanMetadataBranch(ctx, t, localDir)
	t.Chdir(localDir)

	repo, err := git.PlainOpen(localDir)
	require.NoError(t, err)

	orphanHash := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	require.NotEqual(t, remoteTip, orphanHash, "test setup: orphan must differ from remote tip")

	ok, err := healEmptyOrphanFromURL(ctx, repo, remoteDir)
	require.NoError(t, err)
	assert.True(t, ok, "heal should report success when it replaces an empty orphan with real data")

	healed := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	assert.Equal(t, remoteTip, healed,
		"empty orphan should be replaced by the exact remote tip, not a replay with an extra empty commit")
	assert.Contains(t, checkpointRemoteMetadataFiles(ctx, t, localDir), "ab/cabcabcabc/metadata.json",
		"healed branch should contain the remote checkpoint data")
	assert.False(t, testutil.BranchExists(t, localDir, "refs/entire-fetch-tmp/"+paths.MetadataBranchName),
		"temp fetch ref should be cleaned up")
}

// TestBootstrapMetadataFromCheckpointRemote_NoConfigReturnsFalse verifies the
// resolver wrapper is a no-op (and creates no branch) when no checkpoint_remote is
// configured, so the caller falls through to normal orphan creation.
//
// Not parallel: uses t.Chdir().
func TestBootstrapMetadataFromCheckpointRemote_NoConfigReturnsFalse(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, paths.SettingsFileName),
		[]byte(`{"enabled": true}`),
		0o644,
	))
	t.Chdir(localDir)

	repo, err := git.PlainOpen(localDir)
	require.NoError(t, err)

	ok, err := BootstrapMetadataFromCheckpointRemote(t.Context(), repo)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.False(t, testutil.BranchExists(t, localDir, paths.MetadataBranchName),
		"bootstrap must not create a branch when no checkpoint_remote is configured")
}

// TestHealEmptyOrphanMetadataFromCheckpointRemote_NonEmptyLocalReturnsFalse
// verifies that a local metadata branch with real data is never healed/replaced,
// regardless of any configured checkpoint_remote (the guard returns before any
// network access).
//
// Not parallel: uses t.Chdir().
func TestHealEmptyOrphanMetadataFromCheckpointRemote_NonEmptyLocalReturnsFalse(t *testing.T) {
	ctx := context.Background()

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	localDefault := checkpointRemoteCurrentBranch(ctx, t, localDir)
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, localDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, localDir, "abcabcabcabc", "real")
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", localDefault)
	t.Chdir(localDir)

	repo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)

	ok, err := HealEmptyOrphanMetadataFromCheckpointRemote(ctx, repo, localRef)
	require.NoError(t, err)
	assert.False(t, ok, "a non-empty local metadata branch must not be replaced")
}

// TestHealEmptyOrphanMetadataFromCheckpointRemote_HealsVercelOnlyOrphan verifies
// that a local metadata branch carrying only vercel.json — the orphan-init state in
// a vercel-enabled repo (issue #1374) — is still recognized as un-initialized and
// healed from the checkpoint remote. A literal empty-tree check would have skipped
// it because the tree is not empty.
//
// Not parallel: overrides the URL resolver seam and uses t.Chdir().
func TestHealEmptyOrphanMetadataFromCheckpointRemote_HealsVercelOnlyOrphan(t *testing.T) {
	ctx := context.Background()

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")
	remoteDefault := checkpointRemoteCurrentBranch(ctx, t, remoteDir)
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, remoteDir, "rm", "-rf", ".")
	commitCheckpointRemoteMetadata(ctx, t, remoteDir, "abcabcabcabc", "data")
	runCheckpointRemoteGit(ctx, t, remoteDir, "checkout", remoteDefault)
	remoteTip := checkpointRemoteRevParse(ctx, t, remoteDir, paths.MetadataBranchName)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	localDefault := checkpointRemoteCurrentBranch(ctx, t, localDir)
	// Orphan that carries only vercel.json — the vercel-enabled bug state.
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, localDir, "rm", "-rf", ".")
	testutil.WriteFile(t, localDir, vercelconfig.FileName, `{"git":{"deploymentEnabled":{"entire/**":false}}}`)
	runCheckpointRemoteGit(ctx, t, localDir, "add", vercelconfig.FileName)
	runCheckpointRemoteGit(ctx, t, localDir, "commit", "-m", "Initialize metadata branch")
	runCheckpointRemoteGit(ctx, t, localDir, "checkout", localDefault)
	t.Chdir(localDir)

	origResolve := resolveCheckpointRemoteURLFn
	t.Cleanup(func() { resolveCheckpointRemoteURLFn = origResolve })
	resolveCheckpointRemoteURLFn = func(context.Context) (string, bool) { return remoteDir, true }

	repo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)

	ok, err := HealEmptyOrphanMetadataFromCheckpointRemote(ctx, repo, localRef)
	require.NoError(t, err)
	assert.True(t, ok, "a vercel.json-only orphan should be treated as un-initialized and healed")
	assert.Equal(t, remoteTip, checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName),
		"local branch should adopt the checkpoint remote tip")
}

// TestEnsurePrimaryRef_CheckpointRemoteTakesPrecedenceOverOrigin verifies that
// when a checkpoint_remote is configured, EnsurePrimaryRef adopts its branch even
// when a (stale) origin/entire/checkpoints/v1 tracking ref is present. Origin is no
// longer the authoritative checkpoint store, so it must not be seeded from (issue
// #1374).
//
// Not parallel: overrides the URL resolver seam and uses t.Chdir().
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

	// Local clone of origin: origin tracking ref present, no local metadata branch.
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")
	runCheckpointRemoteGit(ctx, t, localDir, "remote", "add", "origin", originDir)
	runCheckpointRemoteGit(ctx, t, localDir, "fetch", "origin")
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, ".entire", paths.SettingsFileName),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))
	t.Chdir(localDir)

	// Point the resolver at the local checkpoint remote instead of the github URL.
	origResolve := resolveCheckpointRemoteURLFn
	t.Cleanup(func() { resolveCheckpointRemoteURLFn = origResolve })
	resolveCheckpointRemoteURLFn = func(context.Context) (string, bool) { return checkpointRemoteDir, true }

	repo, err := git.PlainOpen(localDir)
	require.NoError(t, err)

	// Sanity: the stale origin tracking ref exists and differs from the remote.
	originRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), true)
	require.NoError(t, err)
	require.NotEqual(t, cpTip, originRef.Hash().String(), "test setup: origin must differ from checkpoint remote")

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	got := checkpointRemoteRevParse(ctx, t, localDir, paths.MetadataBranchName)
	assert.Equal(t, cpTip, got, "local branch should adopt the checkpoint remote, not the stale origin ref")
	files := checkpointRemoteMetadataFiles(ctx, t, localDir)
	assert.Contains(t, files, "aa/aaaaaaaaaa/metadata.json", "authoritative checkpoint-remote data should be present")
	assert.NotContains(t, files, "bb/bbbbbbbbbb/metadata.json", "stale origin data must not be adopted")
}

// createEmptyOrphanMetadataBranch creates an empty-tree orphan entire/checkpoints/v1
// branch in dir, reproducing the issue #1374 bug state, and leaves the default
// branch checked out.
func createEmptyOrphanMetadataBranch(ctx context.Context, t *testing.T, dir string) {
	t.Helper()
	defaultBranch := checkpointRemoteCurrentBranch(ctx, t, dir)
	runCheckpointRemoteGit(ctx, t, dir, "checkout", "--orphan", paths.MetadataBranchName)
	runCheckpointRemoteGit(ctx, t, dir, "rm", "-rf", ".")
	runCheckpointRemoteGit(ctx, t, dir, "commit", "--allow-empty", "-m", "Initialize metadata branch")
	runCheckpointRemoteGit(ctx, t, dir, "checkout", defaultBranch)
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
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func checkpointRemoteRevParse(ctx context.Context, t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", rev)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
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
