package codex

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHookPathAPIs_NormalCheckoutKeepDistinctTypes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initCommittedRepo(t, root)

	discovery := resolveHookDiscovery(root)
	worktreeHooks, err := resolveWorktreeHooksPath(root)
	require.NoError(t, err)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, root), discovery.DiscoveredHooks.Path())
	require.Equal(t, canonicalHooksPath(t, root), worktreeHooks.Path())
	require.NotEqual(t, reflect.TypeOf(discovery.DiscoveredHooks), reflect.TypeOf(worktreeHooks))
}

func TestHookPathAPIs_LinkedWorktreeSeparateDiscoveryFromOwnership(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)

	discovery := resolveHookDiscovery(linkedRoot)
	worktreeHooks, err := resolveWorktreeHooksPath(linkedRoot)
	require.NoError(t, err)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, mainRoot), discovery.DiscoveredHooks.Path())
	require.Equal(t, canonicalHooksPath(t, linkedRoot), worktreeHooks.Path())
	require.NotEqual(t, discovery.DiscoveredHooks.Path(), worktreeHooks.Path())
}

func TestHookPathAPIs_UnresolvedDiscoveryKeepsWorktreeOwnership(t *testing.T) {
	t.Parallel()

	featureRoot := setupBareWorktreeLayout(t)

	discovery := resolveHookDiscovery(featureRoot)
	worktreeHooks, err := resolveWorktreeHooksPath(featureRoot)
	require.NoError(t, err)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, filepath.Dir(featureRoot)), discovery.DiscoveredHooks.Path())
	require.Equal(t, canonicalHooksPath(t, featureRoot), worktreeHooks.Path())
}
