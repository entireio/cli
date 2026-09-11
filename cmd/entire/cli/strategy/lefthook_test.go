package strategy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// newLefthookRepo is an isolated repository that looks Lefthook-managed.
// mainConfig names its Lefthook config; empty means lefthook.yml.
func newLefthookRepo(t *testing.T, mainConfig string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	if mainConfig == "" {
		mainConfig = "lefthook.yml"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, mainConfig), []byte("pre-commit: {}\n"), 0o644))
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	return dir
}

// Install writes Entire's own config, one extends entry, and a script per
// hook — and nothing into Lefthook's own config.
func TestEnsureLefthookIntegration(t *testing.T) {
	dir := newLefthookRepo(t, "")
	mainBefore, err := os.ReadFile(filepath.Join(dir, "lefthook.yml"))
	require.NoError(t, err)

	written, err := EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)
	require.Positive(t, written)

	owned, err := os.ReadFile(filepath.Join(dir, entireLefthookConfig))
	require.NoError(t, err)
	require.Contains(t, string(owned), lefthookOwnedMarker)
	require.Contains(t, string(owned), "source_dir_local: "+lefthookScriptDir)

	local, err := os.ReadFile(filepath.Join(dir, "lefthook-local.yml"))
	require.NoError(t, err)
	require.Contains(t, string(local), entireLefthookConfig)
	require.Equal(t, 1, strings.Count(string(local), entireLefthookConfig))

	for _, hook := range gitHookNames {
		script, err := os.ReadFile(filepath.Join(dir, lefthookScriptPath(hook)))
		require.NoError(t, err, hook)
		require.Contains(t, string(script), lefthookOwnedMarker, hook)
		require.Contains(t, string(script), "hooks git "+hook, hook)
		info, err := os.Stat(filepath.Join(dir, lefthookScriptPath(hook)))
		require.NoError(t, err)
		require.NotZero(t, info.Mode().Perm()&0o111, "%s must be executable", hook)
	}

	mainAfter, err := os.ReadFile(filepath.Join(dir, "lefthook.yml"))
	require.NoError(t, err)
	require.Equal(t, mainBefore, mainAfter, "Lefthook's own config must not be touched")

	current, err := LefthookIntegrationCurrent(t.Context(), false)
	require.NoError(t, err)
	require.True(t, current)

	// Repeat installs are no-ops.
	written, err = EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)
	require.Zero(t, written)
}

// Entire never reads Lefthook's main config, so its format and location are
// irrelevant — .toml and .config/ repos work like any other.
func TestEnsureLefthookIntegration_MainConfigFormatIsIrrelevant(t *testing.T) {
	for _, name := range []string{"lefthook.toml", "lefthook.json", ".lefthook.yaml"} {
		t.Run(name, func(t *testing.T) {
			newLefthookRepo(t, name)
			_, err := EnsureLefthookIntegration(t.Context(), false)
			require.NoError(t, err)
			current, err := LefthookIntegrationCurrent(t.Context(), false)
			require.NoError(t, err)
			require.True(t, current)
		})
	}
}

// The extends entry goes into the user's local config without disturbing it.
func TestEnsureLefthookIntegration_PreservesLocalConfig(t *testing.T) {
	for _, tc := range []struct {
		name, local string
		want        []string
	}{
		{"existing content", "# keep me\npre-commit:\n  commands:\n    mine:\n      run: true\n",
			[]string{"# keep me", "mine"}},
		{"existing extends", "extends:\n  - user.yml\n", []string{"user.yml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newLefthookRepo(t, "")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "lefthook-local.yml"), []byte(tc.local), 0o644))
			_, err := EnsureLefthookIntegration(t.Context(), false)
			require.NoError(t, err)

			got, err := os.ReadFile(filepath.Join(dir, "lefthook-local.yml"))
			require.NoError(t, err)
			require.Contains(t, string(got), entireLefthookConfig)
			for _, want := range tc.want {
				require.Contains(t, string(got), want)
			}
		})
	}
}

// lefthook-local.yml takes precedence over lefthook-local.toml, so creating
// the former beside a user's latter would silently stop their hooks running.
func TestEnsureLefthookIntegration_RefusesToShadowANonYAMLLocalConfig(t *testing.T) {
	dir := newLefthookRepo(t, "")
	tomlPath := filepath.Join(dir, "lefthook-local.toml")
	original := []byte("[pre-commit.commands.mine]\nrun = \"true\"\n")
	require.NoError(t, os.WriteFile(tomlPath, original, 0o644))

	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.ErrorIs(t, err, ErrLefthookLocalConfigUnwritable)

	after, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	require.Equal(t, original, after)
	_, statErr := os.Stat(filepath.Join(dir, "lefthook-local.yml"))
	require.True(t, os.IsNotExist(statErr), "must not create a shadowing lefthook-local.yml")
}

// A file Entire did not write is displaced, never destroyed. Corruption
// destroys the marker, so a damaged script would otherwise read as foreign and
// could never be repaired.
func TestEnsureLefthookIntegration_DisplacesUnownedFiles(t *testing.T) {
	dir := newLefthookRepo(t, "")
	scriptPath := filepath.Join(dir, lefthookScriptPath("pre-push"))
	require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
	theirs := []byte("#!/bin/sh\necho mine\n")
	require.NoError(t, os.WriteFile(scriptPath, theirs, 0o755))
	configPath := filepath.Join(dir, entireLefthookConfig)
	theirConfig := []byte("# not Entire's\nkey: value\n")
	require.NoError(t, os.WriteFile(configPath, theirConfig, 0o644))

	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)

	backup, err := os.ReadFile(scriptPath + GitHookBackupSuffix)
	require.NoError(t, err, "the displaced script must be kept")
	require.Equal(t, theirs, backup)
	backup, err = os.ReadFile(configPath + GitHookBackupSuffix)
	require.NoError(t, err, "the displaced config must be kept")
	require.Equal(t, theirConfig, backup)

	current, err := LefthookIntegrationCurrent(t.Context(), false)
	require.NoError(t, err)
	require.True(t, current, "and Entire's own artifacts installed over them")
}

// Uninstall takes only what Entire wrote.
func TestRemoveLefthookIntegration(t *testing.T) {
	dir := newLefthookRepo(t, "")
	localPath := filepath.Join(dir, "lefthook-local.yml")
	require.NoError(t, os.WriteFile(localPath, []byte("pre-commit:\n  commands:\n    mine:\n      run: true\n"), 0o644))
	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)

	removed, err := RemoveLefthookIntegration(t.Context())
	require.NoError(t, err)
	require.Positive(t, removed)

	_, statErr := os.Stat(filepath.Join(dir, entireLefthookConfig))
	require.True(t, os.IsNotExist(statErr))
	local, err := os.ReadFile(localPath)
	require.NoError(t, err, "the user's local config must survive")
	require.NotContains(t, string(local), entireLefthookConfig)
	require.Contains(t, string(local), "mine")

	current, err := LefthookIntegrationCurrent(t.Context(), false)
	require.NoError(t, err)
	require.False(t, current)
}

// A foreign file at Entire's config path is not Entire's to delete.
func TestRemoveLefthookIntegration_LeavesForeignFiles(t *testing.T) {
	dir := newLefthookRepo(t, "")
	configPath := filepath.Join(dir, entireLefthookConfig)
	theirs := []byte("# not Entire's\nkey: value\n")
	require.NoError(t, os.WriteFile(configPath, theirs, 0o644))

	_, err := RemoveLefthookIntegration(t.Context())
	require.NoError(t, err)
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, theirs, after)
}

// Two kinds of wreckage from the era when Entire and Lefthook fought over
// .git/hooks/*: Entire's hook chaining to Lefthook's displaced launcher (which
// runs Entire twice now that Lefthook dispatches it), and the <hook>.old
// backups that make Lefthook fail every later sync (#1349).
func TestEnsureLefthookIntegration_ReconcilesHookFiles(t *testing.T) {
	dir := newLefthookRepo(t, "")
	hooksDir := filepath.Join(dir, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))

	launcher := []byte("#!/bin/sh\ncall_lefthook run \"pre-push\" \"$@\"\n")
	entireHook := []byte("#!/bin/sh\n# " + entireHookMarker + "\nentire hooks git pre-push \"$1\"\n")
	write := func(name string, body []byte) {
		require.NoError(t, os.WriteFile(filepath.Join(hooksDir, name), body, 0o755))
	}
	write("pre-push", entireHook)
	write("pre-push"+GitHookBackupSuffix, launcher)
	write("commit-msg.old", entireHook)
	// Another tool's backup, and a foreign hook Entire displaced: neither is
	// Entire's to touch.
	theirs := []byte("#!/bin/sh\necho someone else\n")
	write("post-commit.old", theirs)
	write("post-rewrite"+GitHookBackupSuffix, theirs)

	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(hooksDir, "pre-push"))
	require.NoError(t, err)
	require.Equal(t, launcher, got, "Lefthook's launcher must be restored over Entire's hook")
	_, statErr := os.Stat(filepath.Join(hooksDir, "pre-push"+GitHookBackupSuffix))
	require.True(t, os.IsNotExist(statErr), "the backup is consumed by the restore")

	_, statErr = os.Stat(filepath.Join(hooksDir, "commit-msg.old"))
	require.True(t, os.IsNotExist(statErr), "Entire's stale wrapper backup must be cleared")

	for name, want := range map[string][]byte{
		"post-commit.old":                    theirs,
		"post-rewrite" + GitHookBackupSuffix: theirs,
	} {
		got, err := os.ReadFile(filepath.Join(hooksDir, name))
		require.NoError(t, err, name)
		require.Equal(t, want, got, "%s belongs to someone else", name)
	}

	// And the native install leaves the yielded hook alone rather than
	// clobbering the launcher and running Entire twice.
	ClearHooksDirCache()
	_, err = ReinstallGitHooks(t.Context())
	require.NoError(t, err)
	got, err = os.ReadFile(filepath.Join(hooksDir, "pre-push"))
	require.NoError(t, err)
	require.Equal(t, launcher, got, "InstallGitHook must skip a hook Lefthook owns")
	got, err = os.ReadFile(filepath.Join(hooksDir, "post-commit"))
	require.NoError(t, err)
	require.Contains(t, string(got), entireHookMarker,
		"a hook Lefthook has not taken over is still Entire's to install")
}

// Delivery is judged on the integration in a Lefthook repo, because Lefthook
// owns .git/hooks/* and rewrites them constantly.
func TestCheckHookDelivery_Lefthook(t *testing.T) {
	newLefthookRepo(t, "")
	got := CheckHookDelivery(t.Context(), false)
	require.False(t, got.OK, "not registered yet")
	require.Equal(t, "Lefthook", got.Manager)
	require.NotEmpty(t, got.Reason)

	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)
	got = CheckHookDelivery(t.Context(), false)
	require.True(t, got.OK)
	require.Equal(t, "Lefthook", got.Manager)
	require.Empty(t, got.Reason)
}

// In a repo with no hook manager, delivery is the native hook state.
func TestCheckHookDelivery_Native(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	ClearHooksDirCache()

	got := CheckHookDelivery(t.Context(), false)
	require.False(t, got.OK)
	require.Empty(t, got.Manager)

	if _, err := ReinstallGitHooks(t.Context()); err != nil {
		t.Fatalf("ReinstallGitHooks: %v", err)
	}
	ClearHooksDirCache()
	got = CheckHookDelivery(t.Context(), false)
	require.True(t, got.OK)
	require.Empty(t, got.Manager)
}

// Entire's artifacts are generated and per-clone, so they go in
// .git/info/exclude rather than the user's .gitignore — and a stale block from
// an earlier version is rewritten rather than appended to.
func TestEnsureLefthookIntegration_ExcludesArtifacts(t *testing.T) {
	dir := newLefthookRepo(t, "")
	excludePath := filepath.Join(dir, ".git", "info", "exclude")
	require.NoError(t, os.MkdirAll(filepath.Dir(excludePath), 0o755))
	require.NoError(t, os.WriteFile(excludePath,
		[]byte("# user's own\n/scratch\n"+excludeBlockBegin+"/stale-entry\n"+excludeBlockEnd), 0o644))

	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)

	got, err := os.ReadFile(excludePath)
	require.NoError(t, err)
	require.Contains(t, string(got), "/scratch", "the user's own entries must survive")
	require.NotContains(t, string(got), "/stale-entry", "the previous block must be replaced")
	require.Equal(t, 1, strings.Count(string(got), excludeBlockBegin), "exactly one block")
	for _, entry := range []string{
		"/" + entireLefthookConfig,
		"/" + lefthookScriptDir + "/",
		"/" + lefthookLocalConfigNames[0],
	} {
		require.Contains(t, string(got), entry+"\n")
	}

	// An install that predates an entry is not "current", so it repairs itself.
	require.NoError(t, os.WriteFile(excludePath,
		[]byte(excludeBlockBegin+"/"+entireLefthookConfig+"\n"+excludeBlockEnd), 0o644))
	current, err := LefthookIntegrationCurrent(t.Context(), false)
	require.NoError(t, err)
	require.False(t, current, "a partial exclude block must trigger a repair")

	_, err = EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)
	current, err = LefthookIntegrationCurrent(t.Context(), false)
	require.NoError(t, err)
	require.True(t, current)

	// Uninstall takes the block back out and leaves the user's entries.
	_, err = RemoveLefthookIntegration(t.Context())
	require.NoError(t, err)
	got, err = os.ReadFile(excludePath)
	require.NoError(t, err)
	require.NotContains(t, string(got), excludeBlockBegin)
	require.NotContains(t, string(got), entireLefthookConfig)

	// And the script directory it created, since nothing else is in it.
	_, statErr := os.Stat(filepath.Join(dir, lefthookScriptDir))
	require.True(t, os.IsNotExist(statErr), "the empty script dir must be pruned")
}

// Entire creates lefthook-local.yml itself, so it must never be the thing that
// makes a repo look Lefthook-managed — otherwise removing Lefthook leaves
// status claiming delivery "via Lefthook" with nothing running Entire.
func TestLefthookManaged(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		file string
		want bool
	}{
		{"lefthook.yml", true},
		{".lefthook.yaml", true},
		{"lefthook.toml", true},
		{"lefthook-local.yml", false},
		{".lefthook-local.toml", false},
		{"package.json", false},
	} {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, tc.file), []byte(""), 0o644))
			require.Equal(t, tc.want, LefthookManaged(dir))
		})
	}
}

// Entire will not bury an existing backup to claim a path: that backup is the
// only copy of whatever was displaced before.
func TestEnsureLefthookIntegration_RefusesToBuryABackup(t *testing.T) {
	dir := newLefthookRepo(t, "")
	scriptPath := filepath.Join(dir, lefthookScriptPath("pre-push"))
	require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
	require.NoError(t, os.WriteFile(scriptPath, []byte("#!/bin/sh\necho newer\n"), 0o755))
	older := []byte("#!/bin/sh\necho older\n")
	require.NoError(t, os.WriteFile(scriptPath+GitHookBackupSuffix, older, 0o755))

	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.ErrorIs(t, err, ErrLefthookArtifactBlocked)

	got, err := os.ReadFile(scriptPath + GitHookBackupSuffix)
	require.NoError(t, err)
	require.Equal(t, older, got, "the existing backup must be untouched")
}

// A visible install that installed nothing must not claim otherwise.
func TestInstallGitHook_ReportsLefthookDeliveryInsteadOfAFalseInstall(t *testing.T) {
	dir := newLefthookRepo(t, "")
	_, err := EnsureLefthookIntegration(t.Context(), false)
	require.NoError(t, err)

	hooksDir := filepath.Join(dir, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	for _, hook := range gitHookNames {
		require.NoError(t, os.WriteFile(filepath.Join(hooksDir, hook),
			[]byte("#!/bin/sh\ncall_lefthook run \""+hook+"\" \"$@\"\n"), 0o755))
	}
	ClearHooksDirCache()

	out := captureStdout(t, func() {
		_, err := InstallGitHook(t.Context(), false, false)
		require.NoError(t, err)
	})
	require.Contains(t, out, "run through Lefthook")
	require.NotContains(t, out, "Installed git hooks")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	fn()
	require.NoError(t, w.Close())
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}
