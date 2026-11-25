package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func userPiHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "pi", "agent")
	require.NoError(t, os.MkdirAll(home, 0o750))
	t.Setenv(piHomeEnvVar, home)
	return home
}

// Install writes the user-level render once, inspect reports it current with
// its version, and removal takes the file and its directory away.
func TestUserHooks_InstallIsIdempotentAndRemovable(t *testing.T) {
	home := userPiHome(t)
	ag := &PiAgent{}
	ctx := context.Background()

	n, path, err := ag.EnsureUserHooks(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, filepath.Join(home, "extensions", "entire", "index.ts"), path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "Entire user-level extension, hook version 1")
	assert.Contains(t, string(data), entireMarker)

	n, _, err = ag.EnsureUserHooks(ctx, nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	report := ag.InspectUserHooks(ctx)
	assert.Equal(t, agent.UserHooksCurrent, report.State)
	assert.Equal(t, userHooksVersion, report.Version)

	removed, _, err := ag.RemoveUserHooks(ctx)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.NoDirExists(t, filepath.Dir(path))
	assert.Equal(t, agent.UserHooksAbsent, ag.InspectUserHooks(ctx).State)
}

// The project render keeps no trace of the user-level placeholders, so
// committed project extensions stay current.
func TestRenderExtension_ProjectRenderHasNoUserLevelCode(t *testing.T) {
	t.Parallel()
	project := renderExtension()
	assert.NotContains(t, project, "__ENTIRE_USER_LEVEL")
	assert.NotContains(t, project, "user-level extension")
	assert.Contains(t, renderExtensionFor(true), "process.cwd()")
}
