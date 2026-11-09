//go:build e2e

package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanCurrentHead(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/clean.md about cleaning stale session data. Do not commit the file. Do not ask for confirmation or approval, just make the change.")
		require.NoError(t, err, "agent failed")

		testutil.AssertFileExists(t, s.Dir, "docs/clean.md")
		testutil.WaitForSessionIdle(t, s.Dir, 30*time.Second)

		sessionStatesBefore := sessionStateFiles(t, s.Dir)
		require.NotEmpty(t, sessionStatesBefore, "expected session state files before clean")

		// A shadow branch an older CLI version left behind: nothing writes
		// these anymore, and `entire clean` removes them.
		const legacyBranch = "entire/1234567-abcdef"
		s.Git(t, "branch", legacyBranch)

		dryRunOut := entire.CleanDryRun(t, s.Dir)
		assert.Contains(t, dryRunOut, "Would clean the following items:")
		assert.Contains(t, dryRunOut, "Session states")
		assert.Contains(t, dryRunOut, "Legacy shadow branches")
		assert.Contains(t, dryRunOut, legacyBranch)
		assert.Contains(t, dryRunOut, "Run without --dry-run to clean these items.")

		assert.ElementsMatch(t, sessionStatesBefore, sessionStateFiles(t, s.Dir),
			"dry-run should not delete session state files")
		_, err = testutil.GitOutputErr(s.Dir, "rev-parse", "--verify", "refs/heads/"+legacyBranch)
		require.NoError(t, err, "dry-run should not delete the legacy shadow branch")

		cleanOut := entire.CleanForce(t, s.Dir)
		assert.Contains(t, cleanOut, "Cleared session state")
		assert.Contains(t, cleanOut, "Deleted legacy shadow branch "+legacyBranch)

		assert.Empty(t, sessionStateFiles(t, s.Dir), "clean should remove current HEAD session state files")
		_, err = testutil.GitOutputErr(s.Dir, "rev-parse", "--verify", "refs/heads/"+legacyBranch)
		assert.Error(t, err, "clean should delete the legacy shadow branch")
	})
}

func sessionStateFiles(t *testing.T, dir string) []string {
	t.Helper()

	stateDir := filepath.Join(dir, ".git", "entire-sessions")
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)

	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		files = append(files, entry.Name())
	}
	return files
}
