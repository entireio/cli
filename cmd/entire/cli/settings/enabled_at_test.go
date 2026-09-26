package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// IsSetUpAndEnabledAt answers for a worktree the process is not in, so a hook
// can decide before moving whether the target is somewhere Entire may run.
func TestIsSetUpAndEnabledAt(t *testing.T) {
	t.Parallel()
	withSettings := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", SettingsName), []byte(body), 0o600))
		return dir
	}
	require.True(t, IsSetUpAndEnabledAt(context.Background(), withSettings(t, `{"enabled": true}`)))
	require.False(t, IsSetUpAndEnabledAt(context.Background(), withSettings(t, `{"enabled": false}`)), "disabled stays out")
	require.False(t, IsSetUpAndEnabledAt(context.Background(), t.TempDir()), "a worktree without .entire was never enabled")
	require.False(t, IsSetUpAndEnabledAt(context.Background(), filepath.Join(t.TempDir(), "missing")), "a missing directory is not enabled either")
}
