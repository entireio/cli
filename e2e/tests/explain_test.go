//go:build e2e

package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExplainCheckpoint shows a checkpoint's intent in the active suite mode.
func TestExplainCheckpoint(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")

		// Commit files from `entire enable` so the worktree is clean.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")
		s.Git(t, "checkout", "-b", "feature")

		prompt := "create a file at docs/feature.md with a short paragraph about feature modules. Do not ask for confirmation or approval, just make the change."
		_, err := s.RunPrompt(t, ctx, prompt)
		require.NoError(t, err, "agent failed")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add feature module doc")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		checkpointID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		s.Git(t, "checkout", mainBranch)

		out := entire.Explain(t, s.Dir, checkpointID)
		assert.Contains(t, out, "● Checkpoint "+checkpointID, "explain output should include the checkpoint ID")
		assert.Contains(t, out, "feature module", "explain output should include the checkpoint intent")
	})
}

// TestExplainCheckpointFromClonedRepo verifies that explain fetches checkpoint
// metadata on demand in the active suite mode when a fresh clone lacks the
// local checkpoint refs.
func TestExplainCheckpointFromClonedRepo(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		bareDir := testutil.SetupBareRemote(t, s)

		// Commit files from `entire enable` so the worktree is clean.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")
		s.Git(t, "push")

		s.Git(t, "checkout", "-b", "feature")

		prompt := "create a file at docs/remote.md with a short paragraph about remote checkpoints. Do not ask for confirmation or approval, just make the change."
		_, err := s.RunPrompt(t, ctx, prompt)
		require.NoError(t, err, "agent failed")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add remote checkpoint doc")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		checkpointID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		s.Git(t, "push", "-u", "origin", "feature")
		testutil.PushCheckpointRefs(t, s.Dir)

		cloneDir := t.TempDir()
		if resolved, symErr := filepath.EvalSymlinks(cloneDir); symErr == nil {
			cloneDir = resolved
		}
		require.NoError(t, os.RemoveAll(cloneDir))
		testutil.Git(t, "", "clone", bareDir, cloneDir)
		testutil.Git(t, cloneDir, "config", "user.name", "E2E Clone")
		testutil.Git(t, cloneDir, "config", "user.email", "e2e-clone@test.local")
		_, err = testutil.GitOutputErr(cloneDir, "rev-parse", "--verify", testutil.CheckpointVerifyRef())
		require.Error(t, err, "checkpoint metadata ref should not exist locally in clone before explain")

		entire.Enable(t, cloneDir, s.Agent.EntireAgent())
		testutil.ApplySuiteCheckpointsMode(t, cloneDir)
		testutil.CommitIfDirty(t, cloneDir, "Enable entire in clone")

		out := entire.Explain(t, cloneDir, checkpointID)
		assert.Contains(t, out, "● Checkpoint "+checkpointID, "explain output should include the checkpoint ID")
		assert.Contains(t, out, "remote checkpoint", "explain output should include the checkpoint intent")
	})
}
