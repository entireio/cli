package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runLegacyShadowCheck(t *testing.T, force bool) string {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, checkLegacyShadowBranches(cmd, force))
	return out.String()
}

func localBranchList(t *testing.T, dir string) []string {
	t.Helper()
	out := strings.TrimSpace(testutil.RunGit(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// Doctor reports strict-shape legacy shadow branches with `entire clean` as
// the remedy and, without --force and without a terminal, changes nothing.
// The bare entire/<hex> form is not reported. Not parallel: t.Chdir.
func TestCheckLegacyShadowBranches_ReportsWithoutForce(t *testing.T) {
	setupStopTestRepo(t)
	dir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(paths.ClearWorktreeRootCache)
	for _, branch := range []string{"entire/1234567-abcdef", "entire/fedcba9"} {
		testutil.RunGit(t, dir, "branch", branch)
	}

	out := runLegacyShadowCheck(t, false)
	assert.Contains(t, out, "Legacy shadow branches: 1 FOUND")
	assert.Contains(t, out, "entire/1234567-abcdef")
	assert.NotContains(t, out, "entire/fedcba9")
	assert.Contains(t, out, "entire doctor --force")
	assert.Contains(t, localBranchList(t, dir), "entire/1234567-abcdef", "nothing is deleted without --force")
}

// Under --force doctor deletes them, except a branch checked out in a
// worktree, which `git branch -D` refuses. Not parallel: t.Chdir.
func TestCheckLegacyShadowBranches_ForceDeletesButKeepsCheckedOut(t *testing.T) {
	setupStopTestRepo(t)
	dir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(paths.ClearWorktreeRootCache)
	for _, branch := range []string{"entire/1234567-abcdef", "entire/89abcde0-123456"} {
		testutil.RunGit(t, dir, "branch", branch)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, dir, "worktree", "add", "-q", linked, "entire/89abcde0-123456")

	out := runLegacyShadowCheck(t, true)
	assert.Contains(t, out, "deleted 1 legacy shadow branch(es)")
	assert.Contains(t, out, "Kept 1 branch(es)")

	branches := localBranchList(t, dir)
	assert.NotContains(t, branches, "entire/1234567-abcdef")
	assert.Contains(t, branches, "entire/89abcde0-123456", "a checked-out branch is never deleted")
}

// Not parallel: t.Chdir.
func TestCheckLegacyShadowBranches_None(t *testing.T) {
	setupStopTestRepo(t)
	t.Cleanup(paths.ClearWorktreeRootCache)
	assert.Contains(t, runLegacyShadowCheck(t, false), "✓ Legacy shadow branches: none")
}

// With only bare entire/<hex> branches, doctor does not delete anything but
// must not claim there are none: it points at `entire clean --all --dry-run`.
// Not parallel: t.Chdir.
func TestCheckLegacyShadowBranches_BareFormOnly(t *testing.T) {
	setupStopTestRepo(t)
	dir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.RunGit(t, dir, "branch", "entire/fedcba9")

	out := runLegacyShadowCheck(t, true)
	assert.NotContains(t, out, "none")
	assert.Contains(t, out, "entire clean --all --dry-run")
	assert.Contains(t, localBranchList(t, dir), "entire/fedcba9", "doctor never deletes the bare form")
}

// Deleting refs frees nothing until git prunes their objects; doctor says so.
// Not parallel: t.Chdir.
func TestCheckLegacyShadowBranches_MentionsGitGC(t *testing.T) {
	setupStopTestRepo(t)
	dir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.RunGit(t, dir, "branch", "entire/1234567-abcdef")

	assert.Contains(t, runLegacyShadowCheck(t, false), "git gc")
	assert.Contains(t, runLegacyShadowCheck(t, true), "git gc")
}

// `entire status` shows a warning row (and --json a count) for legacy shadow
// branches, so they are visible without running doctor. Not parallel: t.Chdir.
func TestRunStatus_WarnsAboutLegacyShadowBranches(t *testing.T) {
	setupStopTestRepo(t)
	dir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(paths.ClearWorktreeRootCache)
	writeSettings(t, testSettingsEnabled)
	testutil.RunGit(t, dir, "branch", "entire/1234567-abcdef")

	var stdout bytes.Buffer
	require.NoError(t, runStatus(context.Background(), &stdout, false, false))
	assert.Contains(t, stdout.String(), "1 legacy shadow branches")
	assert.Contains(t, stdout.String(), "run 'entire doctor'")

	stdout.Reset()
	require.NoError(t, runStatus(context.Background(), &stdout, false, true))
	var got statusJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, 1, got.LegacyShadowBranches)
}

// Uninstall handles a legacy branch git refuses to delete like doctor does: it
// reports the deleted count, lists the kept branch, and does not fail.
// Not parallel: t.Chdir.
func TestUninstallShadowBranches_KeepsRefusedBranchWithoutFailing(t *testing.T) {
	setupStopTestRepo(t)
	dir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(paths.ClearWorktreeRootCache)
	for _, branch := range []string{"entire/1234567-abcdef", "entire/89abcde0-123456"} {
		testutil.RunGit(t, dir, "branch", branch)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, dir, "worktree", "add", "-q", linked, "entire/89abcde0-123456")

	var out, errOut bytes.Buffer
	ok := uninstallShadowBranches(context.Background(), newUninstallPrinter(&out, &errOut))
	assert.True(t, ok, "a refused delete must not fail the uninstall")
	assert.Contains(t, out.String(), "Removed 1 legacy shadow branches")
	assert.Contains(t, errOut.String(), "Kept 1 legacy shadow branch(es)")
	assert.Contains(t, errOut.String(), "entire/89abcde0-123456")
	assert.Contains(t, localBranchList(t, dir), "entire/89abcde0-123456")
}

// Not parallel: t.Chdir.
func TestUninstallShadowBranches_NoneSaysLegacy(t *testing.T) {
	setupStopTestRepo(t)
	t.Cleanup(paths.ClearWorktreeRootCache)
	var out, errOut bytes.Buffer
	assert.True(t, uninstallShadowBranches(context.Background(), newUninstallPrinter(&out, &errOut)))
	assert.Contains(t, out.String(), "No legacy shadow branches to remove")
}
