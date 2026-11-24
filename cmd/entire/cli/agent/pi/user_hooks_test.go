package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// An older user-level render is rewritten; a file that is not Entire's is
// refused and kept.
func TestUserHooks_UpgradesOursAndRefusesForeign(t *testing.T) {
	userPiHome(t)
	ag := &PiAgent{}
	ctx := context.Background()
	_, path, err := ag.EnsureUserHooks(ctx, nil)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte("// "+entireMarker+"\nold\n"), 0o644))
	assert.Equal(t, agent.UserHooksOutdated, ag.InspectUserHooks(ctx).State)
	n, _, err := ag.EnsureUserHooks(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	require.NoError(t, os.WriteFile(path, []byte("export default function () {}\n"), 0o644))
	_, _, err = ag.EnsureUserHooks(ctx, nil)
	require.Error(t, err)
	removed, _, err := ag.RemoveUserHooks(ctx)
	require.NoError(t, err)
	assert.False(t, removed)
	assert.FileExists(t, path)
}

// No Pi home: nothing is created.
func TestUserHooks_NoPiHomeCreatesNothing(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	t.Setenv(piHomeEnvVar, home)
	_, _, err := (&PiAgent{}).EnsureUserHooks(context.Background(), nil)
	require.ErrorIs(t, err, agent.ErrUserHooksNotInUse)
	assert.NoDirExists(t, home)
}

// The project render keeps no trace of the user-level placeholders, so
// committed project extensions stay current.
func TestRenderExtension_ProjectRenderHasNoUserLevelCode(t *testing.T) {
	t.Parallel()
	project := renderExtension()
	assert.NotContains(t, project, "__ENTIRE_USER_LEVEL")
	assert.NotContains(t, project, "user-level extension")
	assert.True(t, strings.Contains(renderExtensionFor(true), "process.cwd()"))
}
