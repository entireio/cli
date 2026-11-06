package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/review"
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
	promptInstallCodexDiscoveredHooks = func(_ context.Context, _ io.Writer, target codex.DiscoveredHooksPath) (bool, error) {
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

// The offer is optional: a cancelled prompt does not fail the setup that
// called it, writes nothing, and prints the remedy.
func TestFinishCodexHooksSetup_PromptErrorSkipsTheOffer(t *testing.T) {
	primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
	stubCodexDiscoveredHooksPrompt(t, true, context.Canceled)

	var out bytes.Buffer
	require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true))
	require.NoDirExists(t, filepath.Dir(primaryHooks))
	require.Contains(t, out.String(), "Skipped installing Codex hooks in "+primaryHooks)
	require.Contains(t, out.String(), "re-run `entire agent add codex`")
}

// A failed write after a yes (here a .codex symlink created after the offer) warns
// and prints the remedy instead of failing setup halfway.
func TestFinishCodexHooksSetup_FailedWriteWarns(t *testing.T) {
	primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
	// The symlink appears while the prompt is up, after the offer was made.
	elsewhere := t.TempDir()
	prev := promptInstallCodexDiscoveredHooks
	t.Cleanup(func() { promptInstallCodexDiscoveredHooks = prev })
	promptInstallCodexDiscoveredHooks = func(context.Context, io.Writer, codex.DiscoveredHooksPath) (bool, error) {
		if err := os.Symlink(elsewhere, filepath.Dir(primaryHooks)); err != nil {
			t.Skipf("cannot create directory symlink: %v", err)
		}
		return true, nil
	}

	var out bytes.Buffer
	require.NoError(t, finishCodexHooksSetup(context.Background(), &out, &codex.CodexAgent{}, true))
	require.NoFileExists(t, filepath.Join(elsewhere, "hooks.json"))
	require.Contains(t, out.String(), "Could not install Codex hooks in "+primaryHooks)
	require.Contains(t, out.String(), "re-run `entire agent add codex`")
	require.NotContains(t, out.String(), "Installed Entire's Codex hooks")
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

func stubCodexOfferAgentSession(t *testing.T, label string) {
	t.Helper()
	prev := detectCodexOfferAgentSession
	t.Cleanup(func() { detectCodexOfferAgentSession = prev })
	detectCodexOfferAgentSession = func() string { return label }
}

func TestCanOfferCodexDiscoveredInstall(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	stubCodexOfferAgentSession(t, "")
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

// A cancelled command stops setup instead of counting as No.
func TestFinishCodexHooksSetup_CancelledCommandReturnsError(t *testing.T) {
	primaryHooks, _ := enterLinkedWorktreeWithCodexHooks(t)
	stubCodexDiscoveredHooksPrompt(t, false, context.Canceled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	err := finishCodexHooksSetup(ctx, &out, &codex.CodexAgent{}, true)
	require.ErrorIs(t, err, context.Canceled)
	require.NoDirExists(t, filepath.Dir(primaryHooks))
}

// An agent child that holds a TTY can pass CanPromptInteractively; the
// cross-checkout offer also refuses whenever an agent session is detected.
func TestCanOfferCodexDiscoveredInstall_RefusesAgentSessions(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")

	stubCodexOfferAgentSession(t, "a Claude Code session")
	require.False(t, canOfferCodexDiscoveredInstall(EnableOptions{}))

	detectCodexOfferAgentSession = review.DetectAgentSession
	t.Setenv("AI_AGENT", "some-agent")
	require.False(t, canOfferCodexDiscoveredInstall(EnableOptions{}))
}

// Outside the explicit ENTIRE_TEST_TTY=1 path, the answer comes from the
// controlling terminal, never from piped stdin.
func TestPromptInstallCodexDiscoveredHooks_ReadsTheTerminal(t *testing.T) {
	enterLinkedWorktreeWithCodexHooks(t)
	target, ok := codex.InspectHookDiagnostics(context.Background()).DiscoveredInstallCandidate()
	require.True(t, ok)
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv(interactive.EnvTestTTY, "")
	original := openPluginPromptTerminal
	t.Cleanup(func() { openPluginPromptTerminal = original })

	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		return pluginPromptTerminal{in: strings.NewReader("y\n")}, nil
	}
	var out bytes.Buffer
	install, err := promptInstallCodexDiscoveredHooks(context.Background(), &out, target)
	require.NoError(t, err)
	require.True(t, install)
	require.Contains(t, out.String(), "Codex reads hooks from "+target.Path())

	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		return pluginPromptTerminal{}, errors.New("no terminal")
	}
	install, err = promptInstallCodexDiscoveredHooks(context.Background(), &out, target)
	require.Error(t, err)
	require.False(t, install)
}
