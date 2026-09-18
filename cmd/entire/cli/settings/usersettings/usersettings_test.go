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
		us.Redaction.OpenAIPrivacyFilter.TimeoutSeconds = 45
		return nil
	}))

	reloaded, err := Load(t.Context())
	require.NoError(t, err)

	global, ok := reloaded.Block("global")
	require.True(t, ok, "an unknown block must survive a write by a binary that does not decode it")
	assert.JSONEq(t, `{"enabled": true, "trusted_origins": ["github.com/acme/widgets"]}`, string(global))

	prefs, ok := reloaded.Block("preferences")
	require.True(t, ok, "a block owned by the settings package must survive too")
	assert.JSONEq(t, `{"telemetry": false}`, string(prefs))

	assert.Equal(t, 45, reloaded.OPF().TimeoutSeconds, "the edit itself landed")
	assert.Equal(t, "/opt/opf/bin/opf", reloaded.OPF().Command, "and did not disturb its sibling")
}

// A decoded block is strict: an unknown key inside it fails the load rather
// than being ignored. It names an executable, so an older binary must not
// guess at a key it does not understand.
func TestDecodedBlockRejectsUnknownKey(t *testing.T) {
	writeUserFile(t, `{"redaction": {"openai_privacy_filter": {"comand": "/opt/opf"}}}`)

	_, err := Load(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redaction")
}

func TestMissingFileIsAnUnconfiguredTier(t *testing.T) {
	t.Setenv(userdirs.EnvConfigDir, t.TempDir())

	us, err := Load(t.Context())
	require.NoError(t, err, "a missing file is not an error")
	assert.Nil(t, us.OPF())
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

func TestOPFRunSettingsValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		timeout int
		prompt  string
		wantErr string
	}{
		{name: "defaults", timeout: 0, prompt: ""},
		{name: "always", timeout: 30, prompt: "always"},
		{name: "negative timeout", timeout: -1, prompt: "", wantErr: "greater than or equal to 0"},
		{name: "unknown prompt", timeout: 0, prompt: "sometimes", wantErr: "must be one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOPFRunSettings(tc.timeout, tc.prompt)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A JSON null block is "unset", the same as omitting the key — otherwise a
// user clearing a block by writing null would get a decode error.
func TestNullBlockIsUnset(t *testing.T) {
	writeUserFile(t, `{"redaction": null}`)

	us, err := Load(t.Context())
	require.NoError(t, err)
	assert.Nil(t, us.OPF())
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
		us.Redaction = &RedactionConfig{OpenAIPrivacyFilter: &OPFConfig{Command: "/opt/opf"}}
		return nil
	}))

	info, err := os.Lstat(filepath.Join(configDir, FileName))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link must survive the write, not be replaced by a regular file")

	data, err := os.ReadFile(realPath)
	require.NoError(t, err)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Contains(t, string(got["redaction"]), "/opt/opf", "the write landed on the link's target")
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
	require.Equal(t, []string{"github.com/acme/widgets"}, keys, "sanity, with a clean environment")

	// Exactly what git hands a hook running in the other repository.
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	keys, present, err = OriginKeys(t.Context(), target)
	require.NoError(t, err)
	require.True(t, present)
	assert.Equal(t, []string{"github.com/acme/widgets"}, keys,
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
