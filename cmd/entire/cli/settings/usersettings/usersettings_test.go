package usersettings

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// writeUserFile points the user config directory at a temp dir and writes
// content there. t.Setenv makes this process-global, so these tests do not
// call t.Parallel.
func writeUserFile(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600))
}

// A block this binary does not decode is still handed back to its owner:
// the settings package reads `preferences` and `repos` this way, and a newer
// binary's blocks must not make the file unreadable.
func TestUnknownBlocksAreReadVerbatim(t *testing.T) {
	writeUserFile(t, `{
	  "newer_block": {"enabled": true},
	  "preferences": {"telemetry": false}
	}`)

	us, err := Load(t.Context())
	require.NoError(t, err)

	newer, ok := us.Block("newer_block")
	require.True(t, ok)
	assert.JSONEq(t, `{"enabled": true}`, string(newer))
	prefs, ok := us.Block("preferences")
	require.True(t, ok)
	assert.JSONEq(t, `{"telemetry": false}`, string(prefs))
}

func TestMissingFileIsAnUnconfiguredTier(t *testing.T) {
	t.Setenv(userdirs.EnvConfigDir, t.TempDir())

	us, err := Load(t.Context())
	require.NoError(t, err, "a missing file is not an error")
	_, ok := us.Block("preferences")
	assert.False(t, ok)
}

// Reading only the first value would silently honor half a configuration.
func TestTrailingJSONIsRejected(t *testing.T) {
	writeUserFile(t, `{"redaction": {}}{"redaction": {}}`)

	_, err := Load(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple JSON values")
}

// A `repos` path key and a worktree root can spell the same directory
// differently. On macOS /tmp is itself a symlink, which is the ordinary case.
func TestPathIsRootMatchesThroughASymlink(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(target, link))

	assert.True(t, PathIsRoot(link, target), "a linked key must match the real root")
	assert.True(t, PathIsRoot(target, link), "and the reverse")
	assert.False(t, PathIsRoot(t.TempDir(), target), "an unrelated directory must not match")
}

func TestExpandTildeRefusesRelativePaths(t *testing.T) {
	t.Parallel()
	_, err := ExpandTilde("some/relative/path")
	require.Error(t, err, "a relative key would mean different repositories depending on cwd")
	assert.Contains(t, err.Error(), "absolute")
}

// A symlinked settings file is refused, the same as a symlinked
// .entire/settings.local.json: the read must not follow a link to whatever it
// names.
func TestLoadRefusesASymlinkedSettingsFile(t *testing.T) {
	realPath := filepath.Join(t.TempDir(), "elsewhere.json")
	require.NoError(t, os.WriteFile(realPath, []byte(`{"preferences":{"telemetry":true}}`), 0o600))

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	require.NoError(t, os.Symlink(realPath, filepath.Join(configDir, FileName)))

	_, err := Load(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	assert.Contains(t, err.Error(), "symbolic link")
	assert.Contains(t, err.Error(), realPath, "the message names the link target")
}

// A config directory that does not exist yet is an unconfigured tier, and
// reading it must not create it.
func TestLoadDoesNotCreateTheConfigDirectory(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "missing")
	t.Setenv(userdirs.EnvConfigDir, configDir)

	us, err := Load(t.Context())
	require.NoError(t, err)
	_, ok := us.Block("preferences")
	assert.False(t, ok)
	assert.NoDirExists(t, configDir)
}

func TestNormalizeOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "direct GitHub", url: "https://github.com/entireio/cli.git", want: "gh/entireio/cli"},
		{name: "mixed-case GitHub", url: "git@GitHub.com:entireio/cli.git", want: "gh/entireio/cli"},
		{name: "Entire GitHub mirror", url: "entire://aws-us-east-2.entire.io/gh/entireio/cli", want: "gh/entireio/cli"},
		{name: "Entire native US region", url: "entire://aws-us-east-2.entire.io/et/acme/widgets", want: "et/acme/widgets"},
		{name: "Entire native EU region", url: "entire://eu-west-1.entire.io/et/acme/widgets", want: "et/acme/widgets"},
		{name: "GitLab fallback", url: "https://gitlab.com/acme/widgets.git", want: "gitlab.com/acme/widgets"},
		{name: "self-hosted fallback", url: "ssh://git@git.corp.example/acme/widgets.git", want: "git.corp.example/acme/widgets"},
		{name: "unknown Entire forge fallback", url: "entire://cluster.example/jk/acme/widgets", want: "cluster.example/acme/widgets"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, NormalizeOrigin(tt.url))
		})
	}
}

func TestOriginKeysCollapsesGitHubTransports(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runGit(t, root, "init", "-q", ".")
	runGit(t, root, "remote", "add", "origin", "https://github.com/entireio/cli.git")
	runGit(t, root, "config", "--add", "remote.origin.pushurl",
		"entire://aws-us-east-2.entire.io/gh/entireio/cli")

	keys, present, err := OriginKeys(t.Context(), root)
	require.NoError(t, err)
	require.True(t, present)
	assert.Equal(t, []string{"gh/entireio/cli"}, keys)
}

// OriginKeys runs on the hook path: IsSetUpAndEnabled reaches it, and git
// exports GIT_DIR and GIT_WORK_TREE to every hook it runs. Those OUTRANK
// cmd.Dir, so a child that inherits them reads the hook's repository instead
// of the one named — and for a per-repository settings lookup that means
// matching another repository's entry and applying its configuration here.
//
// No source guard covers this class; the existing one is specific to `git
// status` and --no-optional-locks. So it gets a behavioural test.
func TestOriginKeysIgnoresHookRepoOverrides(t *testing.T) {
	target := t.TempDir()
	runGit(t, target, "init", "-q", ".")
	runGit(t, target, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	other := t.TempDir()
	runGit(t, other, "init", "-q", ".")
	runGit(t, other, "remote", "add", "origin", "https://github.com/other/thing.git")

	keys, present, err := OriginKeys(t.Context(), target)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, []string{"gh/acme/widgets"}, keys, "sanity, with a clean environment")

	// Exactly what git hands a hook running in the other repository.
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	keys, present, err = OriginKeys(t.Context(), target)
	require.NoError(t, err)
	require.True(t, present)
	assert.Equal(t, []string{"gh/acme/widgets"}, keys,
		"the directory asked about must win over a hook's exported repository")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}
