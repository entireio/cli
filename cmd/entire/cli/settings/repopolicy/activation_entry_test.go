package repopolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadRepoActivation_ProjectEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePolicyFile(t, root, ".entire/configuration.json", `{"enabled":true}`)
	if err := os.Symlink("configuration.json", filepath.Join(root, ".entire", "settings.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := ReadRepoActivation(t.Context(), root)
	require.Error(t, err)
}

func TestReadRepoActivation_IgnoredLocalEntry(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire", "settings.local.json"), 0o700))
	stubLocalSettingsVerdict(t, LocalSettingsTracked)
	activation, err := ReadRepoActivation(t.Context(), root)
	require.NoError(t, err)
	require.False(t, activation.Configured)
}
