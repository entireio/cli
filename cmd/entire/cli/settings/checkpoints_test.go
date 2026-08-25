package settings

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCheckpointsSettingsRepo creates a tmp repo (with a .git dir so
// paths.AbsPath resolves) and chdirs into it. Not parallel: uses t.Chdir.
func newCheckpointsSettingsRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	t.Chdir(dir)
	return dir
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", name), []byte(body), 0o644))
}

func TestLoadCheckpointsConfig_AbsentIsNil(t *testing.T) {
	newCheckpointsSettingsRepo(t)
	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadCheckpointsConfig_NoBlockIsNil(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	writeFile(t, dir, "settings.json", `{"enabled": true}`)
	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadCheckpointsConfig_ParsesPrimaryAndMirrors(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	writeFile(t, dir, "settings.json", `{
		"enabled": true,
		"checkpoints": {
			"primary": {"type": "git"},
			"mirrors": [{"type": "fs", "config": {"path": "/tmp/x"}}]
		}
	}`)
	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "git", cfg.Primary.Type)
	require.Len(t, cfg.Mirrors, 1)
	assert.Equal(t, "fs", cfg.Mirrors[0].Type)
	assert.JSONEq(t, `{"path": "/tmp/x"}`, string(cfg.Mirrors[0].Config))
}

func TestLoadCheckpointsConfig_EnvOverridesPrimary(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	// A file block that the env override must replace wholesale.
	writeFile(t, dir, "settings.json", `{"enabled": true, "checkpoints": {"primary": {"type": "git-branch"}}}`)
	t.Setenv(EnvCheckpointsPrimary, "git-refs")

	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "git-refs", cfg.Primary.Type)
	assert.Empty(t, cfg.Mirrors)
}

func TestLoadCheckpointsConfig_EnvPrimaryAndMirrors(t *testing.T) {
	newCheckpointsSettingsRepo(t)
	t.Setenv(EnvCheckpointsPrimary, "git-refs")
	t.Setenv(EnvCheckpointsMirrors, "git-branch, fs")

	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "git-refs", cfg.Primary.Type)
	require.Len(t, cfg.Mirrors, 2)
	assert.Equal(t, "git-branch", cfg.Mirrors[0].Type)
	assert.Equal(t, "fs", cfg.Mirrors[1].Type)
}

func TestLoadCheckpointsConfig_EmptyEnvFallsBackToFile(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	writeFile(t, dir, "settings.json", `{"enabled": true, "checkpoints": {"primary": {"type": "git-branch"}}}`)
	t.Setenv(EnvCheckpointsPrimary, "")

	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "git-branch", cfg.Primary.Type, "empty env override defers to the settings file")
}

func TestLoadCheckpointsConfig_RejectsUnknownFieldInBlock(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	writeFile(t, dir, "settings.json", `{"enabled": true, "checkpoints": {"primary": {"type": "git"}, "bogus": 1}}`)
	_, err := LoadCheckpointsConfig(context.Background())
	require.Error(t, err)
}

func TestLoadCheckpointsConfig_InvalidWhenPrimaryTypeMissing(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	writeFile(t, dir, "settings.json", `{"enabled": true, "checkpoints": {"primary": {}}}`)
	_, err := LoadCheckpointsConfig(context.Background())
	require.ErrorIs(t, err, ErrInvalidCheckpointsConfig)
}

func TestLoadCheckpointsConfig_ToleratesUnrelatedMalformedSettings(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	// Unrelated field is the wrong shape and there is no checkpoints block:
	// the loader must stay fail-soft and return nil rather than erroring.
	writeFile(t, dir, "settings.json", `{"enabled": true, "summary_generation": "not-an-object"}`)
	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadCheckpointsConfig_ToleratesWholeFileSyntaxError(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	writeFile(t, dir, "settings.json", `{"enabled": true,,}`)
	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadCheckpointsConfig_LocalOverridesInvalidBase(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	// Base has an invalid checkpoints block; local has a valid one. Since local
	// replaces base wholesale, the base's invalidity must not block the load.
	writeFile(t, dir, "settings.json", `{"enabled": true, "checkpoints": {"primary": {}}}`)
	writeFile(t, dir, "settings.local.json", `{"checkpoints": {"primary": {"type": "git"}}}`)

	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "git", cfg.Primary.Type)
}

func TestLoadCheckpointsConfig_RejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	dir := newCheckpointsSettingsRepo(t)

	// A valid checkpoints config that lives OUTSIDE the .entire directory.
	outside := filepath.Join(t.TempDir(), "evil.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"checkpoints": {"primary": {"type": "git-branch"}}}`), 0o644))
	// Point .entire/settings.json at it via an (absolute) symlink that escapes
	// the directory. The confined read must refuse to follow it, so the config
	// is not picked up and we fail soft to the default.
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, ".entire", "settings.json")))

	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cfg, "an escaping symlink must not be followed; config should fail soft to nil")
}

func TestLoadCheckpointsConfig_ToleratesUnreadableFile(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	// settings.json as a directory makes os.ReadFile fail with a non-ENOENT
	// error; the loader must stay fail-soft (no new failure for Open).
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire", "settings.json"), 0o755))
	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadCheckpointsConfig_LocalOverridesBase(t *testing.T) {
	dir := newCheckpointsSettingsRepo(t)
	writeFile(t, dir, "settings.json", `{"enabled": true, "checkpoints": {"primary": {"type": "git"}, "mirrors": [{"type": "fs"}]}}`)
	writeFile(t, dir, "settings.local.json", `{"checkpoints": {"primary": {"type": "git"}}}`)

	cfg, err := LoadCheckpointsConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	// Local block replaces the base block wholesale, so the base's mirror is gone.
	assert.Equal(t, "git", cfg.Primary.Type)
	assert.Empty(t, cfg.Mirrors)
}
