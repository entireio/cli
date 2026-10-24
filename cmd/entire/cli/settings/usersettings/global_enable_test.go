package usersettings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

func TestSetGlobalEnable_CreatesTheFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "entire")
	t.Setenv(userdirs.EnvConfigDir, dir)

	path, err := SetGlobalEnable(t.Context(), true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, FileName), path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"global_enable": true}`, string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// Every other block — the settings package's and a newer binary's — survives
// the write.
func TestSetGlobalEnable_KeepsEveryOtherBlock(t *testing.T) {
	writeUserFile(t, `{
	  "preferences": {"telemetry": false},
	  "repos": {"github/acme/app": {"log_level": "debug"}},
	  "newer_block": {"x": [1, 2]},
	  "global_enable": true
	}`)

	path, err := SetGlobalEnable(t.Context(), false)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	  "preferences": {"telemetry": false},
	  "repos": {"github/acme/app": {"log_level": "debug"}},
	  "newer_block": {"x": [1, 2]},
	  "global_enable": false
	}`, string(data))
}

// A file that does not parse is never overwritten: the write would discard
// whatever the user was in the middle of editing.
func TestSetGlobalEnable_RefusesAMalformedFile(t *testing.T) {
	for name, content := range map[string]string{
		"invalid JSON":  `{"preferences": `,
		"two values":    `{"preferences": {}} {"global_enable": true}`,
		"not an object": `[true]`,
	} {
		t.Run(name, func(t *testing.T) {
			writeUserFile(t, content)
			path := filepath.Join(os.Getenv(userdirs.EnvConfigDir), FileName)

			_, err := SetGlobalEnable(t.Context(), true)
			require.Error(t, err)

			data, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, content, string(data))
		})
	}
}

// The file is refused when it is a symlink, as every read of it is.
func TestSetGlobalEnable_RefusesASymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, dir)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	require.NoError(t, os.WriteFile(target, []byte(`{}`), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, FileName)))

	_, err := SetGlobalEnable(t.Context(), true)
	require.Error(t, err)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(data))
}
