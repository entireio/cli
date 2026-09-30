package usersettings

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// The property the decision to build this alongside the separate global-tier
// work rests on: a block this binary does not know is preserved byte for byte
// across a read-modify-write. Without it, a binary carrying only this change
// would silently delete a `global` block the other work wrote, and the two
// could not share one file on disk.
func TestUnknownBlocksSurviveReadModifyWrite(t *testing.T) {
	writeUserFile(t, `{
	  "global": {"enabled": true, "trusted_origins": ["github.com/acme/widgets"]},
	  "preferences": {"telemetry": false},
	  "redaction": {"openai_privacy_filter": {"command": "/opt/opf/bin/opf"}}
	}`)

	require.NoError(t, Modify(t.Context(), func(us *UserSettings) error {
		us.SetBlock("preferences", json.RawMessage(`{"telemetry": true}`))
		return nil
	}))

	reloaded, err := Load(t.Context())
	require.NoError(t, err)

	global, ok := reloaded.Block("global")
	require.True(t, ok, "an unknown block must survive a write by a binary that does not decode it")
	assert.JSONEq(t, `{"enabled": true, "trusted_origins": ["github.com/acme/widgets"]}`, string(global))

	prefs, ok := reloaded.Block("preferences")
	require.True(t, ok)
	assert.JSONEq(t, `{"telemetry": true}`, string(prefs), "the edit itself landed")

	redaction, ok := reloaded.Block("redaction")
	require.True(t, ok, "a sibling block must survive an edit to another one")
	assert.Contains(t, string(redaction), "/opt/opf/bin/opf")
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

// The write must land on a symlink's target, not replace the link: ~/.config
// managed by a dotfiles tool is the population this file serves.
func TestWriteFollowsASymlinkedSettingsFile(t *testing.T) {
	realDir := t.TempDir()
	realPath := filepath.Join(realDir, "dotfiles-settings.json")
	require.NoError(t, os.WriteFile(realPath, []byte(`{"redaction":{}}`), 0o600))

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	require.NoError(t, os.Symlink(realPath, filepath.Join(configDir, FileName)))

	require.NoError(t, Modify(t.Context(), func(us *UserSettings) error {
		us.SetBlock("preferences", json.RawMessage(`{"telemetry": true}`))
		return nil
	}))

	info, err := os.Lstat(filepath.Join(configDir, FileName))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link must survive the write, not be replaced by a regular file")

	data, err := os.ReadFile(realPath)
	require.NoError(t, err)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Contains(t, string(got["preferences"]), "telemetry", "the write landed on the link's target")
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
