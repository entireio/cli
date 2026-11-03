package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/require"
)

func setClaudeHookVersion(t *testing.T, version string, err error) {
	t.Helper()
	previous := installedClaudeHookVersion
	installedClaudeHookVersion = func(context.Context) (string, error) { return version, err }
	t.Cleanup(func() { installedClaudeHookVersion = previous })
}

// These flows change CWD and the executable probe, so they cannot run in parallel.
func TestClaudeHookVersionBatchRefusesBeforeAnyHookChanges(t *testing.T) {
	for _, mode := range []string{"enable_add", "enable_remove", "manage_add", "manage_remove", "manage_reinstall", "hook_set", "hook_set_force"} {
		t.Run(mode, func(t *testing.T) {
			dir := setupTestRepo(t)
			ctx := context.Background()
			claude, err := agent.Get(agent.AgentNameClaudeCode)
			require.NoError(t, err)
			cursor, err := agent.Get(agent.AgentNameCursor)
			require.NoError(t, err)
			selected := []agent.Agent{cursor, claude}
			var installed []types.AgentName
			originals := map[string][]byte{}
			if mode == "enable_remove" || mode == "manage_remove" {
				_, err = setupAgentHooks(ctx, cursor, false)
				require.NoError(t, err)
				selected = []agent.Agent{claude}
				installed = []types.AgentName{cursor.Name()}
			}
			if mode == "manage_reinstall" {
				_, err = setupAgentHooks(ctx, claude, false)
				require.NoError(t, err)
				installed = []types.AgentName{claude.Name()}
			}
			for _, path := range []string{".cursor/hooks.json", ".claude/settings.json"} {
				data, readErr := os.ReadFile(filepath.Join(dir, path))
				if readErr == nil {
					originals[path] = data
				} else {
					require.ErrorIs(t, readErr, os.ErrNotExist)
				}
			}
			setClaudeHookVersion(t, "v2.1.72", nil)
			var out bytes.Buffer
			opts := EnableOptions{Yes: true, ForceHooks: mode == "manage_reinstall" || mode == "hook_set_force"}
			switch mode {
			case "enable_add", "enable_remove":
				err = runEnableInteractive(ctx, &out, selected, opts)
			case "hook_set", "hook_set_force":
				added, errs := setupAgentHookSet(ctx, &out, selected, opts.ForceHooks)
				require.Empty(t, added)
				err = errors.Join(errs...)
			default:
				names := make([]string, 0, len(selected))
				for _, ag := range selected {
					names = append(names, string(ag.Name()))
				}
				err = applyAgentChanges(ctx, &out, names, installed, opts)
			}
			require.ErrorContains(t, err, "2.1.78 or later")
			for _, path := range []string{".cursor/hooks.json", ".claude/settings.json"} {
				if original, exists := originals[path]; exists {
					actual, readErr := os.ReadFile(filepath.Join(dir, path))
					require.NoError(t, readErr)
					require.Equal(t, original, actual)
				} else {
					require.NoFileExists(t, filepath.Join(dir, path))
				}
			}
			require.NoFileExists(t, filepath.Join(dir, EntireSettingsFile))
		})
	}
}

func TestClaudeHookVersionBatchProbesOnce(t *testing.T) {
	for _, mode := range []string{"enable", "manage", "hook_set"} {
		t.Run(mode, func(t *testing.T) {
			setupTestRepo(t)
			ctx := context.Background()
			claude, err := agent.Get(agent.AgentNameClaudeCode)
			require.NoError(t, err)
			cursor, err := agent.Get(agent.AgentNameCursor)
			require.NoError(t, err)
			if mode == "manage" {
				_, err = setupAgentHooks(ctx, claude, false)
				require.NoError(t, err)
			}
			previous := installedClaudeHookVersion
			calls := 0
			installedClaudeHookVersion = func(context.Context) (string, error) {
				calls++
				if calls > 1 {
					return "", errors.New("version probe repeated after mutations began")
				}
				return minimumClaudeHookVersion, nil
			}
			t.Cleanup(func() { installedClaudeHookVersion = previous })
			var out bytes.Buffer
			selected := []agent.Agent{cursor, claude}
			switch mode {
			case "enable":
				err = runEnableInteractive(ctx, &out, selected, EnableOptions{Yes: true})
			case "manage":
				err = applyAgentChanges(ctx, &out, []string{string(cursor.Name()), string(claude.Name())},
					[]types.AgentName{claude.Name()}, EnableOptions{Yes: true, ForceHooks: true})
			default:
				added, errs := setupAgentHookSet(ctx, &out, selected, false)
				require.Len(t, added, 2)
				err = errors.Join(errs...)
			}
			require.NoError(t, err, out.String())
			require.Equal(t, 1, calls)
		})
	}
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
