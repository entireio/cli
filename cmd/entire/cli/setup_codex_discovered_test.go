package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// stubCodexDiscoveredHooksPrompt replaces the consent prompt and records the
// hooks paths it was asked about.
func stubCodexDiscoveredHooksPrompt(t *testing.T, answer bool, err error) *[]string {
	t.Helper()
	prev := promptInstallCodexDiscoveredHooks
	t.Cleanup(func() { promptInstallCodexDiscoveredHooks = prev })
	var asked []string
	promptInstallCodexDiscoveredHooks = func(_ context.Context, target codex.DiscoveredHooksPath) (bool, error) {
		asked = append(asked, target.Path())
		return answer, err
	}
	return &asked
}

// enterLinkedWorktreeWithCodexHooks creates a primary checkout plus a linked
// worktree, installs Entire's Codex hooks in the worktree only, and returns
// the primary checkout's hooks path.
func enterLinkedWorktreeWithCodexHooks(t *testing.T) (primaryHooks, worktreeHooks string) {
	t.Helper()
	tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	t.Chdir(linkedRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))
	_, err := (&codex.CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	return resolvedHooksPath(t, repoRoot), resolvedHooksPath(t, linkedRoot)
}

func requireEntireCodexHooks(t *testing.T, hooksPath string) {
	t.Helper()
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "entire hooks codex stop")
}

func TestFinishCodexHooksSetup_YesInstallsIntoDiscoveredRoot(t *testing.T) {
	primaryHooks, worktreeHooks := enterLinkedWorktreeWithCodexHooks(t)
	asked := stubCodexDiscoveredHooksPrompt(t, true, nil)

	var out bytes.Buffer
	require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true))

	require.Equal(t, []string{primaryHooks}, *asked)
	requireEntireCodexHooks(t, primaryHooks)
	requireEntireCodexHooks(t, worktreeHooks)
	require.Contains(t, out.String(), "Installed Entire's Codex hooks in "+primaryHooks+".")
	require.Contains(t, out.String(), "This is an uncommitted change in that checkout.")
	require.Contains(t, out.String(), "Open /hooks in Codex to approve them.")
	require.NotContains(t, out.String(), "re-run `entire agent add codex`")
	require.NotContains(t, out.String(), "will not run here")

	// The discovered file is now current: running setup again does not ask.
	out.Reset()
	require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true))
	require.Len(t, *asked, 1)
	require.Contains(t, out.String(), "Entire's Codex hooks are already installed there.")
}

func TestFinishCodexHooksSetup_NoConsentWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mayPrompt  bool
		wantPrompt bool
	}{
		{name: "declined", mayPrompt: true, wantPrompt: true},
		{name: "cannot prompt", mayPrompt: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
			asked := stubCodexDiscoveredHooksPrompt(t, false, nil)

			var out bytes.Buffer
			require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, tc.mayPrompt))

			require.Equal(t, tc.wantPrompt, len(*asked) == 1)
			require.NoDirExists(t, filepath.Dir(primaryHooks))
			require.Contains(t, out.String(), "Hooks in the current-worktree file will not run here.")
			require.Contains(t, out.String(),
				"From a terminal, re-run `entire agent add codex` in this worktree and answer yes to install Entire's Codex hooks in "+primaryHooks+".")
			require.NotContains(t, out.String(), "cd ")
		})
	}
}

func TestFinishCodexHooksSetup_PromptErrorIsReturned(t *testing.T) {
	primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
	stubCodexDiscoveredHooksPrompt(t, true, context.Canceled)

	var out bytes.Buffer
	err := finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true)
	require.ErrorIs(t, err, context.Canceled)
	require.NoDirExists(t, filepath.Dir(primaryHooks))
}

func TestFinishCodexHooksSetup_OutdatedDiscoveredHooksPrompt(t *testing.T) {
	primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
	stale := `{"hooks":{"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop"}]}]}}`
	writeCodexHooksForDiagnosticTest(t, filepath.Dir(filepath.Dir(primaryHooks)), stale)
	asked := stubCodexDiscoveredHooksPrompt(t, true, nil)

	var out bytes.Buffer
	require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true))
	require.Equal(t, []string{primaryHooks}, *asked)
	requireEntireCodexHooks(t, primaryHooks)
}

func TestFinishCodexHooksSetup_CurrentDiscoveredHooksDoNotPrompt(t *testing.T) {
	primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
	linkedRoot, err := os.Getwd()
	require.NoError(t, err)
	t.Chdir(filepath.Dir(filepath.Dir(primaryHooks)))
	paths.ClearWorktreeRootCache()
	_, err = (&codex.CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	t.Chdir(linkedRoot)
	paths.ClearWorktreeRootCache()
	asked := stubCodexDiscoveredHooksPrompt(t, true, errors.New("must not prompt"))

	var out bytes.Buffer
	require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true))
	require.Empty(t, *asked)
	require.Contains(t, out.String(), "Codex reads hooks from: "+primaryHooks)
	require.Contains(t, out.String(), "Entire's Codex hooks are already installed there.")
}

func TestFinishCodexHooksSetup_NormalCheckoutDoesNotPrompt(t *testing.T) {
	repoRoot := setupTestRepo(t)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))
	_, err := (&codex.CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	asked := stubCodexDiscoveredHooksPrompt(t, true, errors.New("must not prompt"))

	var out bytes.Buffer
	require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true))
	require.Empty(t, *asked)
	require.Empty(t, out.String())
	requireEntireCodexHooks(t, resolvedHooksPath(t, repoRoot))
}

func TestCanOfferCodexDiscoveredInstall(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	require.True(t, canOfferCodexDiscoveredInstall(EnableOptions{}))
	require.False(t, canOfferCodexDiscoveredInstall(EnableOptions{Yes: true}), "--yes never writes another checkout")

	t.Setenv(interactive.EnvTestTTY, "0")
	require.False(t, canOfferCodexDiscoveredInstall(EnableOptions{}), "no terminal never writes another checkout")
}

func TestSetupAgentHooksNonInteractive_YesNeverWritesDiscoveredRoot(t *testing.T) {
	primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
	t.Setenv(interactive.EnvTestTTY, "1")
	asked := stubCodexDiscoveredHooksPrompt(t, true, errors.New("must not prompt"))

	var out bytes.Buffer
	require.NoError(t, setupAgentHooksNonInteractive(context.Background(), &out, &codex.CodexAgent{}, EnableOptions{Yes: true}))
	require.Empty(t, *asked)
	require.NoDirExists(t, filepath.Dir(primaryHooks))
	require.Contains(t, out.String(), "re-run `entire agent add codex` in this worktree")
}
