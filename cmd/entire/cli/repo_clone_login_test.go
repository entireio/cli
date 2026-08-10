package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

const testCloneLoginServer = "https://us.auth.entire.io"

// notLoggedInReauthErr returns a genuine *reauthError (as an opaque error) by
// resolving the control-plane target under an isolated, empty config dir — the
// only public path to the unexported type. Not parallel-safe (sets an env var).
func notLoggedInReauthErr(t *testing.T) error {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	_, err := auth.ResolveControlPlaneTarget()
	require.Error(t, err)
	_, ok := auth.ReauthMessage(err)
	require.True(t, ok, "expected a reauth error from an empty config dir")
	return err
}

func swapProbeCloneSession(t *testing.T, fn func(context.Context) (string, error)) {
	t.Helper()
	orig := probeCloneSession
	probeCloneSession = fn
	t.Cleanup(func() { probeCloneSession = orig })
}

func swapInteractiveReauthLogin(t *testing.T, fn func(*cobra.Command, string, string) (bool, error)) {
	t.Helper()
	orig := interactiveReauthLogin
	interactiveReauthLogin = fn
	t.Cleanup(func() { interactiveReauthLogin = orig })
}

// TestRenderCoreError_StripsReauthNoise verifies the shared choke point surfaces
// only the clean re-login hint, not the ogen/command wrapper noise from the
// reported `entire repo clone` failure.
func TestRenderCoreError_StripsReauthNoise(t *testing.T) {
	re := notLoggedInReauthErr(t)
	msg, ok := auth.ReauthMessage(re)
	require.True(t, ok)

	chain := fmt.Errorf("resolve mirror placements: %w",
		fmt.Errorf("security \"BearerAuth\": %w", re))

	got := renderCoreError(chain)
	require.Error(t, got)
	require.Equal(t, msg, got.Error())
	require.NotContains(t, got.Error(), "resolve mirror placements")
	require.NotContains(t, got.Error(), "BearerAuth")
}

func TestEnsureCloneSession(t *testing.T) {
	newCmd := func(t *testing.T) *cobra.Command {
		cmd := newCloneTestCmd()
		cmd.SetContext(t.Context())
		return cmd
	}

	t.Run("healthy session proceeds without prompting", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "")
		swapProbeCloneSession(t, func(context.Context) (string, error) {
			return testCloneLoginServer, nil
		})
		swapInteractiveReauthLogin(t, func(*cobra.Command, string, string) (bool, error) {
			t.Fatal("interactiveReauthLogin must not run for a healthy session")
			return false, nil
		})
		require.NoError(t, ensureCloneSession(newCmd(t)))
	})

	t.Run("non-interactive reauth returns the clean hint, no login", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "")
		re := notLoggedInReauthErr(t)
		msg, _ := auth.ReauthMessage(re)
		swapProbeCloneSession(t, func(context.Context) (string, error) {
			return testCloneLoginServer, re
		})
		swapInteractiveReauthLogin(t, func(*cobra.Command, string, string) (bool, error) {
			t.Fatal("interactiveReauthLogin must not run without a TTY")
			return false, nil
		})
		// Default go test is non-interactive, so no prompt is attempted.
		err := ensureCloneSession(newCmd(t))
		require.Error(t, err)
		require.Equal(t, msg, err.Error())
		require.NotContains(t, err.Error(), "BearerAuth")
	})

	t.Run("transient probe error passes through unchanged", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "")
		transient := errors.New("dial tcp: connection refused")
		swapProbeCloneSession(t, func(context.Context) (string, error) {
			return testCloneLoginServer, transient
		})
		swapInteractiveReauthLogin(t, func(*cobra.Command, string, string) (bool, error) {
			t.Fatal("interactiveReauthLogin must not run for a transient error")
			return false, nil
		})
		err := ensureCloneSession(newCmd(t))
		require.ErrorIs(t, err, transient)
	})

	t.Run("non-empty ENTIRE_TOKEN skips the probe", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "tok")
		swapProbeCloneSession(t, func(context.Context) (string, error) {
			t.Fatal("probe must not run when ENTIRE_TOKEN is set")
			return "", nil
		})
		require.NoError(t, ensureCloneSession(newCmd(t)))
	})

	t.Run("interactive reauth logs in and continues", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "")
		t.Setenv(interactive.EnvTestTTY, "1")
		re := notLoggedInReauthErr(t)
		msg, _ := auth.ReauthMessage(re)
		swapProbeCloneSession(t, func(context.Context) (string, error) {
			return testCloneLoginServer, re
		})
		var gotServer, gotHint string
		called := false
		swapInteractiveReauthLogin(t, func(_ *cobra.Command, server, hint string) (bool, error) {
			called = true
			gotServer, gotHint = server, hint
			return true, nil // logged in
		})
		require.NoError(t, ensureCloneSession(newCmd(t)))
		require.True(t, called)
		require.Equal(t, testCloneLoginServer, gotServer)
		require.Equal(t, msg, gotHint)
	})

	t.Run("interactive decline returns the clean hint", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "")
		t.Setenv(interactive.EnvTestTTY, "1")
		re := notLoggedInReauthErr(t)
		msg, _ := auth.ReauthMessage(re)
		swapProbeCloneSession(t, func(context.Context) (string, error) {
			return testCloneLoginServer, re
		})
		swapInteractiveReauthLogin(t, func(*cobra.Command, string, string) (bool, error) {
			return false, nil // user declined
		})
		err := ensureCloneSession(newCmd(t))
		require.Error(t, err)
		require.Equal(t, msg, err.Error())
	})

	t.Run("interactive login error propagates", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "")
		t.Setenv(interactive.EnvTestTTY, "1")
		re := notLoggedInReauthErr(t)
		loginErr := errors.New("login failed")
		swapProbeCloneSession(t, func(context.Context) (string, error) {
			return testCloneLoginServer, re
		})
		swapInteractiveReauthLogin(t, func(*cobra.Command, string, string) (bool, error) {
			return false, loginErr
		})
		err := ensureCloneSession(newCmd(t))
		require.ErrorIs(t, err, loginErr)
	})
}
