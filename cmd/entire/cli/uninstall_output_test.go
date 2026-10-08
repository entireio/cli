package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestUninstallPrinter_MultiLineWarningStaysInItsBlock pins the shape of a
// warning whose message spans several lines. The reason text can be an
// external plugin's own stderr, which is arbitrary: every line has to sit
// under the ⚠ marker, or a chatty plugin's second line reads as a new
// top-level message.
func TestUninstallPrinter_MultiLineWarningStaysInItsBlock(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	p := newUninstallPrinter(&stdout, &stderr)
	p.warnUnder("failed to remove agent hooks: %v", multiLineError{})

	want := []string{
		"    ⚠ failed to remove agent hooks: plugin exploded",
		"      stack frame one",
		"      stack frame two",
	}
	got := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("warnUnder() printed %d lines, want %d:\n%s", len(got), len(want), stderr.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

type multiLineError struct{}

func (multiLineError) Error() string {
	return "plugin exploded\nstack frame one\nstack frame two\n"
}

// Uninstall names each hook it put back, so a user whose own hook was moved
// aside at enable time can see it is live again.
//
// Not parallel: uses t.Chdir().
func TestUninstallGitHooks_ReportsRestoredHook(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	strategy.ClearHooksDirCache()
	t.Cleanup(strategy.ClearHooksDirCache)

	hooksDir := filepath.Join(dir, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o750))
	testutil.RunGit(t, dir, "config", "core.hooksPath", hooksDir)
	require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte("#!/bin/sh\necho mine\n"), 0o700))

	_, err := strategy.InstallGitHook(t.Context(), true, false)
	require.NoError(t, err)

	var stdout, stderr bytes.Buffer
	require.True(t, uninstallGitHooks(t.Context(), newUninstallPrinter(&stdout, &stderr)), stderr.String())

	require.Contains(t, stdout.String(), "Restored your original pre-push hook")
	require.Equal(t, 1, strings.Count(stdout.String(), "Restored your original"), stdout.String())
}
