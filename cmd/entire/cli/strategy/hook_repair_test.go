package strategy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func hookRepairRepo(t *testing.T) (string, string) {
	t.Helper()
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	t.Chdir(repoDir)
	_, err := EnsureGitHookIntegration(t.Context(), false)
	require.NoError(t, err)
	return repoDir, filepath.Join(repoDir, ".git", "hooks")
}

// These tests change CWD because the repair API resolves the current repository.
func TestHookRepair_PreservesUserEdits(t *testing.T) {
	for _, manager := range []string{"native", "lefthook"} {
		for _, position := range []string{"before", "after"} {
			t.Run(manager+"/"+position, func(t *testing.T) {
				repoDir, hooksDir := hookRepairRepo(t)
				if manager == "lefthook" {
					require.NoError(t, os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644))
				}
				hookPath := filepath.Join(hooksDir, "pre-push")
				original, err := os.ReadFile(hookPath)
				require.NoError(t, err)
				edited := string(original) + "echo user-validation\n"
				if position == "before" {
					edited = strings.Replace(string(original), "#!/bin/sh\n", "#!/bin/sh\necho user-validation\n", 1)
				}
				require.NoError(t, os.WriteFile(hookPath, []byte(edited), 0o755))
				for range 2 {
					_, err := EnsureGitHookIntegration(t.Context(), false)
					require.Error(t, err, "repair must refuse a user-edited hook")
					require.Contains(t, err.Error(), "pre-push")
					after, err := os.ReadFile(hookPath)
					require.NoError(t, err)
					require.Equal(t, edited, string(after))
					require.NoFileExists(t, hookPath+backupSuffix)
					health := CheckGitHookIntegration(t.Context())
					require.NotEqual(t, GitHookIntegrationCurrent, health.State)
					require.Contains(t, health.Reason, "pre-push")
				}
			})
		}
	}
}

func TestHookRepair_NativePermissions(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("Windows does not use Unix executable bits")
	}
	_, hooksDir := hookRepairRepo(t)
	hookPath := filepath.Join(hooksDir, "pre-push")
	require.NoError(t, os.Chmod(hookPath, 0o644))
	require.NotEqual(t, GitHookIntegrationCurrent, CheckGitHookIntegration(t.Context()).State)
	written, err := EnsureGitHookIntegration(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, 1, written)
	info, err := os.Stat(hookPath)
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o111)
	require.Equal(t, GitHookIntegrationCurrent, CheckGitHookIntegration(t.Context()).State)
	written, err = EnsureGitHookIntegration(t.Context(), false)
	require.NoError(t, err)
	require.Zero(t, written)
}

func TestHookRepair_PreviousGeneratedPrePush(t *testing.T) {
	_, hooksDir := hookRepairRepo(t)
	hookPath := filepath.Join(hooksDir, "pre-push")
	current, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	previous := strings.Replace(string(current), `pre-push "$1" "$2"`, `pre-push "$1"`, 1)
	require.NotEqual(t, string(current), previous)
	require.NoError(t, os.WriteFile(hookPath, []byte(previous), 0o755))
	_, err = EnsureGitHookIntegration(t.Context(), false)
	require.NoError(t, err)
	after, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	require.Equal(t, string(current), string(after))
}

func TestHookRepair_PreflightsBeforeChangingHooks(t *testing.T) {
	_, hooksDir := hookRepairRepo(t)
	// An earlier hook needs migration, but a later hook contains a user edit.
	// Even direct installer callers must not partially update the hook set.
	first := filepath.Join(hooksDir, "prepare-commit-msg")
	legacy := "#!/bin/sh\n# Entire CLI hooks\n./scripts/entire-dev hooks git prepare-commit-msg\n"
	require.NoError(t, os.WriteFile(first, []byte(legacy), 0o755))
	last := filepath.Join(hooksDir, "pre-push")
	original, err := os.ReadFile(last)
	require.NoError(t, err)
	edited := string(original) + "echo user-validation\n"
	require.NoError(t, os.WriteFile(last, []byte(edited), 0o755))
	backup := "#!/bin/sh\necho existing-backup\n"
	require.NoError(t, os.WriteFile(last+backupSuffix, []byte(backup), 0o755))
	_, err = InstallGitHook(t.Context(), true, false)
	require.Error(t, err)
	for path, expected := range map[string]string{first: legacy, last: edited, last + backupSuffix: backup} {
		actual, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, expected, string(actual))
	}
}
