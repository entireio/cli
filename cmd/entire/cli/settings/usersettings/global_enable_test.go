package usersettings

import (
	"encoding/json"
	"errors"
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

// Modify writes nothing when its function fails: a writer that refuses the
// file's contents must leave them as they were.
func TestModify_WritesNothingWhenTheFunctionFails(t *testing.T) {
	const content = `{"repos": {"gh/acme/app": [1]}}`
	writeUserFile(t, content)
	path := filepath.Join(os.Getenv(userdirs.EnvConfigDir), FileName)

	_, err := Modify(t.Context(), func(blocks map[string]json.RawMessage) error {
		blocks["global_enable"] = json.RawMessage(`true`)
		return errors.New("refused")
	})
	require.ErrorContains(t, err, "refused")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, content, string(data))
}

// Modify hands the function every block and writes back what it leaves,
// creating the file when there is none.
func TestModify_RoundTripsBlocks(t *testing.T) {
	writeUserFile(t, `{"newer_block": {"x": 1}}`)

	path, err := Modify(t.Context(), func(blocks map[string]json.RawMessage) error {
		assert.JSONEq(t, `{"x": 1}`, string(blocks["newer_block"]))
		blocks["repos"] = json.RawMessage(`{"gh/acme/app": {"push_checkpoints": true}}`)
		return nil
	})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"newer_block": {"x": 1}, "repos": {"gh/acme/app": {"push_checkpoints": true}}}`, string(data))
}
