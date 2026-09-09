//go:build e2e

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/require"
)

// TestLefthookRefreshKeepsAgentCommitCheckpointed exercises the user-visible
// failure from issue #2264 end to end: Lefthook takes ownership of every live
// Git hook after Entire was enabled, but the next commit of real agent work
// must still be captured without requiring the user to run `entire status`
// first.
func TestLefthookRefreshKeepsAgentCommitCheckpointed(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		lefthookConfig := filepath.Join(s.Dir, "lefthook.yml")
		require.NoError(t, os.WriteFile(lefthookConfig, []byte("pre-commit: {}\n"), 0o644))
		s.Git(t, "add", "lefthook.yml")
		s.Git(t, "commit", "-m", "Configure Lefthook")

		// Re-enable after Lefthook appears so Entire creates its clone-local
		// configuration and generated scripts while native hooks still bridge
		// the transition.
		entire.Enable(t, s.Dir, s.Agent.EntireAgent())
		s.HeadBefore = testutil.GitOutput(t, s.Dir, "rev-parse", "HEAD")
		s.CheckpointBefore = testutil.CheckpointState(s.Dir)
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/lefthook-survives.md with a paragraph about hooks surviving. Do not commit the file. Do not ask for confirmation, just make the change. Do not use a worktree.")
		require.NoError(t, err)
		testutil.AssertFileExists(t, s.Dir, "docs/lefthook-survives.md")

		// This overwrite is the last action before git add/commit. In particular,
		// no Entire status or lifecycle command may repair the live hooks between
		// the simulated Lefthook refresh and the commit under test.
		hooksDir := testutil.GitOutput(t, s.Dir, "rev-parse", "--git-path", "hooks")
		if !filepath.IsAbs(hooksDir) {
			hooksDir = filepath.Join(s.Dir, hooksDir)
		}
		for _, hook := range []string{"prepare-commit-msg", "commit-msg", "post-commit", "post-rewrite", "pre-push"} {
			require.NoError(t, os.WriteFile(
				filepath.Join(hooksDir, hook),
				[]byte(simulatedLefthookLauncher(hook)),
				0o755,
			))
		}
		s.Git(t, "add", "docs/lefthook-survives.md")
		s.Git(t, "commit", "-m", "Verify Lefthook checkpoint")
		testutil.AssertNewCommits(t, s, 1)
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)
		checkpointID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, checkpointID)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)

		// Inspect health only after the commit/checkpoint assertions above. This
		// proves status observed the durable delivery; it could not have repaired
		// the commit path being tested.
		statusCmd := execx.NonInteractive(ctx, entire.BinPath(), "status", "--json")
		statusCmd.Dir = s.Dir
		statusOut, err := statusCmd.CombinedOutput()
		require.NoError(t, err, "entire status --json failed: %s", statusOut)
		var status struct {
			CheckpointSyncState string `json:"checkpoint_sync_state"`
			GitHooks            struct {
				Mode  string `json:"mode"`
				State string `json:"state"`
			} `json:"git_hooks"`
		}
		require.NoError(t, json.Unmarshal(statusOut, &status))
		require.Equal(t, "ready", status.CheckpointSyncState)
		require.Equal(t, "lefthook", status.GitHooks.Mode)
		require.Equal(t, "current", status.GitHooks.State)
	})
}

// simulatedLefthookLauncher keeps the structure of a Lefthook v2 launcher but
// dispatches directly to the clone-local script. That makes the test
// deterministic and validates the same argv/stdin boundary without requiring a
// separately installed Lefthook binary in every agent E2E environment.
func simulatedLefthookLauncher(hook string) string {
	return fmt.Sprintf(`#!/bin/sh

if [ "$LEFTHOOK_VERBOSE" = "1" -o "$LEFTHOOK_VERBOSE" = "true" ]; then
  set -x
fi

if [ "$LEFTHOOK" = "0" ]; then
  exit 0
fi

call_lefthook()
{
  shift 2
  sh ".lefthook-local/%s/entire.sh" "$@"
}

call_lefthook run "%s" "$@"
`, hook, hook)
}
