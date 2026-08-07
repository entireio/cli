package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// newScanTestRepo returns an initialized repo in its own temp dir. The path is
// symlink-resolved so it matches what the scanner reports.
func newScanTestRepo(t *testing.T) string {
	t.Helper()
	repo := resolvedTempDir(t)
	testutil.InitRepo(t, repo)
	return repo
}

// newScanTestRepoIn initializes a repo named name inside parent and returns its
// absolute path.
func newScanTestRepoIn(t *testing.T, parent, name string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	require.NoError(t, os.MkdirAll(repo, 0o755))
	testutil.InitRepo(t, repo)
	return repo
}

func TestInspectRepoForScan_NotSetUp(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)

	got := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})

	require.Equal(t, repo, got.Path)
	require.False(t, got.SetUp)
	require.False(t, got.Enabled)
	require.False(t, got.GitHooksInstalled)
	require.Empty(t, got.HooksOutdated)
	require.Empty(t, got.CodexTrustGaps)
	require.False(t, got.LinkedWorktree)

	// Assertions on the agent lists name the agent rather than requiring the
	// slices to be empty: the agent registry is process-global, and other tests
	// in this package register their own agents into it.
	require.NotContains(t, got.AgentsHooked, "claude-code")
	require.NotContains(t, got.AgentsDetectedUnhooked, "claude-code")
}

func TestInspectRepoForScan_SetUpButDisabled(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)
	writeScanSettingsFixture(t, repo, `{"enabled": false}`)

	got := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})

	require.True(t, got.SetUp, "a settings.json makes the repo set up")
	require.False(t, got.Enabled, "enabled:false must be reported as disabled")
}

func TestInspectRepoForScan_SetUpAndEnabled(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)
	writeScanSettingsFixture(t, repo, `{"enabled": true}`)

	got := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})

	require.True(t, got.SetUp)
	require.True(t, got.Enabled)
}

func TestInspectRepoForScan_LocalOnlySettingsCountAsSetUp(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".entire", "settings.local.json"), []byte(`{"enabled": true}`), 0o600))

	got := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})

	require.True(t, got.SetUp, "`entire enable --local` writes only settings.local.json")
	require.True(t, got.Enabled)
}

// TestInspectRepoForScan_HookedAgent installs Claude Code's hooks for real —
// under the context root override, without chdir — and asserts the inspection
// sees them from outside the repo. This is the whole point of the override: no
// per-agent change was needed to make AreHooksInstalled work on another repo.
func TestInspectRepoForScan_HookedAgent(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)
	writeScanSettingsFixture(t, repo, `{"enabled": true}`)

	claude, err := agent.Get(types.AgentName("claude-code"))
	require.NoError(t, err)
	hooks, ok := agent.AsHookSupport(claude)
	require.True(t, ok, "claude-code declares hook support")
	_, err = hooks.InstallHooks(paths.WithWorktreeRoot(context.Background(), repo), false, false)
	require.NoError(t, err)

	got := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})

	require.Contains(t, got.AgentsHooked, "claude-code")
	require.NotContains(t, got.AgentsDetectedUnhooked, "claude-code",
		"a hooked agent is never also listed as detected-but-unhooked")
	require.Empty(t, got.HooksOutdated, "freshly installed hooks are current")
}

// TestInspectRepoForScan_PresentButUnhooked covers the repo that motivates
// `entire scan`: the agent is clearly in use, Entire just never got enabled.
func TestInspectRepoForScan_PresentButUnhooked(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".claude"), 0o755))

	got := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})

	require.NotContains(t, got.AgentsHooked, "claude-code")
	require.Contains(t, got.AgentsDetectedUnhooked, "claude-code")
	require.False(t, got.SetUp)
}

func TestInspectRepoForScan_ReportsGitHooks(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)

	before := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})
	require.False(t, before.GitHooksInstalled)

	installScanTestGitHooks(t, repo)

	after := inspectRepoForScan(context.Background(), scanCandidate{Root: repo})
	require.True(t, after.GitHooksInstalled)
}

func TestInspectRepoForScan_CarriesLinkedWorktreeFlag(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)

	got := inspectRepoForScan(context.Background(), scanCandidate{Root: repo, LinkedWorktree: true})

	require.True(t, got.LinkedWorktree)
}

// TestInspectRepoForScan_DoesNotLeakRootIntoCallerContext guards the override's
// blast radius: inspecting a repo must not change how the caller's own context
// resolves paths afterwards.
func TestInspectRepoForScan_DoesNotLeakRootIntoCallerContext(t *testing.T) {
	t.Parallel()

	repo := newScanTestRepo(t)
	other := resolvedTempDir(t)
	ctx := paths.WithWorktreeRoot(context.Background(), other)

	inspectRepoForScan(ctx, scanCandidate{Root: repo})

	root, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)
	require.Equal(t, other, root, "the caller's context is unchanged")
}

func writeScanSettingsFixture(t *testing.T, repo, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".entire", "settings.json"), []byte(contents), 0o600))
}

// installScanTestGitHooks writes every managed git hook into repo's hooks dir
// carrying the Entire marker, which is what IsGitHookInstalledInDir looks for.
func installScanTestGitHooks(t *testing.T, repo string) {
	t.Helper()
	hooksDir := filepath.Join(repo, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	for _, name := range strategy.ManagedGitHookNames() {
		script := "#!/bin/sh\n# Entire CLI hooks\n"
		require.NoError(t, os.WriteFile(filepath.Join(hooksDir, name), []byte(script), 0o600))
	}
}
