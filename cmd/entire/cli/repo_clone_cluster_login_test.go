package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

const testClusterHost = "aws-eu-central-1.entire.io"

func swapProbeClusterCloneSession(t *testing.T, fn func(context.Context, string) ([]string, error)) {
	t.Helper()
	orig := probeClusterCloneSession
	probeClusterCloneSession = fn
	t.Cleanup(func() { probeClusterCloneSession = orig })
}

func swapInteractiveClusterLogin(t *testing.T, fn func(*cobra.Command, []string, string) (bool, error)) {
	t.Helper()
	orig := interactiveClusterLogin
	interactiveClusterLogin = fn
	t.Cleanup(func() { interactiveClusterLogin = orig })
}

func TestEnsureClusterCloneSession(t *testing.T) {
	newCmd := func(t *testing.T) *cobra.Command {
		cmd := newCloneTestCmd()
		cmd.SetContext(t.Context())
		return cmd
	}

	t.Run("usable cluster session proceeds without prompting", func(t *testing.T) {
		unsetCloneEnvToken(t)
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return nil, nil
		})
		swapInteractiveClusterLogin(t, func(*cobra.Command, []string, string) (bool, error) {
			t.Fatal("interactiveClusterLogin must not run for a usable session")
			return false, nil
		})
		require.NoError(t, ensureClusterCloneSession(newCmd(t), testClusterHost))
	})

	t.Run("expired eligible context offers login to its core", func(t *testing.T) {
		unsetCloneEnvToken(t)
		t.Setenv(interactive.EnvTestTTY, "1")
		re := notLoggedInReauthErr(t)
		msg, _ := auth.ReauthMessage(re)
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return []string{testCloneLoginServer}, re
		})
		var gotServers []string
		var gotHint string
		swapInteractiveClusterLogin(t, func(_ *cobra.Command, servers []string, hint string) (bool, error) {
			gotServers, gotHint = servers, hint
			return true, nil
		})
		require.NoError(t, ensureClusterCloneSession(newCmd(t), testClusterHost))
		require.Equal(t, []string{testCloneLoginServer}, gotServers)
		require.Equal(t, msg, gotHint)
	})

	t.Run("no eligible context, non-interactive, names the cores", func(t *testing.T) {
		unsetCloneEnvToken(t)
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return []string{testCloneLoginServer}, auth.ErrNoEligibleContext
		})
		swapInteractiveClusterLogin(t, func(*cobra.Command, []string, string) (bool, error) {
			t.Fatal("interactiveClusterLogin must not run without a TTY")
			return false, nil
		})
		err := ensureClusterCloneSession(newCmd(t), testClusterHost)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no auth context for cluster "+testClusterHost)
		require.Contains(t, err.Error(), testCloneLoginServer)
	})

	t.Run("no eligible context, interactive single core, logs in", func(t *testing.T) {
		unsetCloneEnvToken(t)
		t.Setenv(interactive.EnvTestTTY, "1")
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return []string{testCloneLoginServer}, auth.ErrNoEligibleContext
		})
		var gotServers []string
		swapInteractiveClusterLogin(t, func(_ *cobra.Command, servers []string, _ string) (bool, error) {
			gotServers = servers
			return true, nil
		})
		require.NoError(t, ensureClusterCloneSession(newCmd(t), testClusterHost))
		require.Equal(t, []string{testCloneLoginServer}, gotServers)
	})

	t.Run("no eligible context, interactive multi core, passes all servers", func(t *testing.T) {
		unsetCloneEnvToken(t)
		t.Setenv(interactive.EnvTestTTY, "1")
		cores := []string{testCloneLoginServer, "https://eu.auth.entire.io"}
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return cores, auth.ErrNoEligibleContext
		})
		var gotServers []string
		swapInteractiveClusterLogin(t, func(_ *cobra.Command, servers []string, _ string) (bool, error) {
			gotServers = servers
			return true, nil
		})
		require.NoError(t, ensureClusterCloneSession(newCmd(t), testClusterHost))
		require.Equal(t, cores, gotServers)
	})

	t.Run("ambiguous context is surfaced, never auto-logs-in", func(t *testing.T) {
		unsetCloneEnvToken(t)
		t.Setenv(interactive.EnvTestTTY, "1")
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return nil, auth.ErrAmbiguousContext
		})
		swapInteractiveClusterLogin(t, func(*cobra.Command, []string, string) (bool, error) {
			t.Fatal("interactiveClusterLogin must not run for the ambiguous case")
			return false, nil
		})
		err := ensureClusterCloneSession(newCmd(t), testClusterHost)
		require.ErrorIs(t, err, auth.ErrAmbiguousContext)
	})

	t.Run("discovery error is surfaced, never prompts", func(t *testing.T) {
		unsetCloneEnvToken(t)
		t.Setenv(interactive.EnvTestTTY, "1")
		discErr := errors.New("aws-eu-central-1.entire.io doesn't look like a cluster, or it is unreachable")
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return nil, discErr
		})
		swapInteractiveClusterLogin(t, func(*cobra.Command, []string, string) (bool, error) {
			t.Fatal("interactiveClusterLogin must not run for a discovery error")
			return false, nil
		})
		err := ensureClusterCloneSession(newCmd(t), testClusterHost)
		require.ErrorIs(t, err, discErr)
	})

	t.Run("interactive decline returns a silent clean hint", func(t *testing.T) {
		unsetCloneEnvToken(t)
		t.Setenv(interactive.EnvTestTTY, "1")
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return []string{testCloneLoginServer}, auth.ErrNoEligibleContext
		})
		swapInteractiveClusterLogin(t, func(*cobra.Command, []string, string) (bool, error) {
			return false, nil // declined
		})
		err := ensureClusterCloneSession(newCmd(t), testClusterHost)
		require.Error(t, err)
		var silentErr *SilentError
		require.ErrorAs(t, err, &silentErr)
		require.Contains(t, err.Error(), "no auth context for cluster "+testClusterHost)
	})

	t.Run("interactive login error propagates", func(t *testing.T) {
		unsetCloneEnvToken(t)
		t.Setenv(interactive.EnvTestTTY, "1")
		loginErr := errors.New("login failed")
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			return []string{testCloneLoginServer}, auth.ErrNoEligibleContext
		})
		swapInteractiveClusterLogin(t, func(*cobra.Command, []string, string) (bool, error) {
			return false, loginErr
		})
		err := ensureClusterCloneSession(newCmd(t), testClusterHost)
		require.ErrorIs(t, err, loginErr)
	})

	t.Run("non-empty ENTIRE_TOKEN skips the probe", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "tok")
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			t.Fatal("probe must not run when ENTIRE_TOKEN is set")
			return nil, nil
		})
		require.NoError(t, ensureClusterCloneSession(newCmd(t), testClusterHost))
	})

	t.Run("empty ENTIRE_TOKEN skips the probe", func(t *testing.T) {
		t.Setenv(auth.EnvTokenVar, "")
		swapProbeClusterCloneSession(t, func(context.Context, string) ([]string, error) {
			t.Fatal("probe must not run when ENTIRE_TOKEN is present but empty")
			return nil, nil
		})
		require.NoError(t, ensureClusterCloneSession(newCmd(t), testClusterHost))
	})
}

// TestRepoClone_ClusterSessionWiring asserts each URL form probes the correct
// target cluster before touching git. The probe returns a stub error so RunE
// stops before the real placements lookup / git clone; we only assert the host.
func TestRepoClone_ClusterSessionWiring(t *testing.T) {
	stop := errors.New("stop before clone")

	run := func(t *testing.T, args ...string) string {
		t.Helper()
		unsetCloneEnvToken(t)
		var gotHost string
		swapProbeClusterCloneSession(t, func(_ context.Context, host string) ([]string, error) {
			gotHost = host
			return nil, stop // surfaced verbatim; halts RunE before lookup/git
		})
		cmd := newCloneTestCmd()
		cmd.SetArgs(args)
		require.ErrorIs(t, cmd.ExecuteContext(t.Context()), stop)
		return gotHost
	}

	t.Run("full entire:// URL probes the URL's cluster", func(t *testing.T) {
		got := run(t, "entire://"+testClusterHost+"/gh/gtrrz-victor/testing-rift")
		require.Equal(t, testClusterHost, got)
	})

	t.Run("--cluster probes the flag's cluster", func(t *testing.T) {
		got := run(t, "/gh/gtrrz-victor/testing-rift", "--cluster", testClusterHost)
		require.Equal(t, testClusterHost, got)
	})
}
