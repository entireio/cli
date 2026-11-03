package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/stretchr/testify/require"
)

func setClaudeHookVersion(t *testing.T, version string, err error) {
	t.Helper()
	previous := installedClaudeHookVersion
	installedClaudeHookVersion = func(context.Context) (string, error) { return version, err }
	t.Cleanup(func() { installedClaudeHookVersion = previous })
}

func TestParseClaudeHookVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		output string
		want   string
	}{
		{"2.1.72 (Claude Code)\n", "v2.1.72"},
		{"v2.1.78 (Claude Code)\r\n", "v2.1.78"},
		{"2.1.78-beta.1 (Claude Code)", "v2.1.78-beta.1"},
		{"3.0.0+build (Claude Code)", "v3.0.0+build"},
		{"", ""},
		{"unknown", ""},
		{"error: version 2.1.78 unavailable", ""},
	} {
		t.Run(tc.output, func(t *testing.T) {
			t.Parallel()
			got, err := parseClaudeHookVersion(tc.output)
			if tc.want == "" {
				require.ErrorContains(t, err, "claude --version")
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestProbeClaudeHookVersionMissingAndCanceled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	version, err := probeClaudeHookVersion(context.Background())
	require.NoError(t, err)
	require.Empty(t, version)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = probeClaudeHookVersion(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// These tests replace a process-wide executable probe and cannot run in parallel.
func TestClaudeHookVersionRequirement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		wantErr bool
	}{
		{"older", "v2.1.72", true},
		{"just_before", "v2.1.77", true},
		{"prerelease", "v2.1.78-beta.1", true},
		{"minimum", "v2.1.78", false},
		{"newer", "v2.1.288", false},
		{"future_major", "v3.0.0", false},
		{"not_installed", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setClaudeHookVersion(t, tc.version, nil)
			err := checkClaudeHookVersion(context.Background(), agent.AgentNameClaudeCode)
			if tc.wantErr {
				require.ErrorContains(t, err, "2.1.78 or later")
				require.ErrorContains(t, err, "entire agent add claude-code")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestClaudeHookVersionProbeFailureDoesNotPass(t *testing.T) {
	probeErr := errors.New("version probe failed")
	setClaudeHookVersion(t, "", probeErr)
	require.ErrorIs(t, checkClaudeHookVersion(context.Background(), agent.AgentNameClaudeCode), probeErr)
	// Other agents do not depend on a working Claude executable.
	require.NoError(t, checkClaudeHookVersion(context.Background(), agent.AgentNameCodex))
}

func TestInstallAgentHooksRefusesOldClaudeWithoutChangingSettings(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			t.Run(testCaseName(existing, force), func(t *testing.T) {
				dir := setupGitRepoForPhaseTest(t)
				t.Chdir(dir)
				path := filepath.Join(dir, ".claude", "settings.json")
				original := []byte(`{"model":"user-choice","hooks":{"Stop":[{"hooks":[{"type":"command","command":"entire hooks claude-code stop"}]}]}}`)
				if existing {
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
					require.NoError(t, os.WriteFile(path, original, 0o600))
				}
				setClaudeHookVersion(t, "v2.1.72", nil)
				count, pruned, err := installAgentHooks(context.Background(), &claudecode.ClaudeCodeAgent{}, force)
				require.ErrorContains(t, err, "2.1.72")
				require.Zero(t, count)
				require.False(t, pruned)
				if existing {
					actual, readErr := os.ReadFile(path)
					require.NoError(t, readErr)
					require.Equal(t, original, actual)
				} else {
					require.NoDirExists(t, filepath.Dir(path))
				}
			})
		}
	}
}

func testCaseName(existing, force bool) string {
	name := "fresh"
	if existing {
		name = "existing"
	}
	if force {
		name += "_force"
	}
	return name
}

func TestDoctorReportsIncompatibleClaudeInsteadOfHealthyHooks(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	_, err := (&claudecode.ClaudeCodeAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	path := filepath.Join(dir, ".claude", "settings.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	setClaudeHookVersion(t, "v2.1.72", nil)
	cmd, stdout := newTestCmd(t)
	checkHookDrift(cmd)
	require.Contains(t, stdout.String(), "Claude Code hooks: VERSION CHECK FAILED")
	require.Contains(t, stdout.String(), "2.1.78 or later")
	require.NotContains(t, stdout.String(), "Claude Code hook config: OK")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
