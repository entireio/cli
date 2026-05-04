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

// TestResumeFromClonedRepo: agent creates a file on a feature branch and user
// commits, then the repo is cloned (simulating a teammate). The clone has no
// local checkpoint metadata ref for the active suite mode. `entire resume feature`
// should fetch that ref automatically and restore the session.
func TestResumeFromClonedRepo(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Set up a bare remote so we can push and clone.
		bareDir := testutil.SetupBareRemote(t, s)

		// Commit files from `entire enable` so main has a clean working tree.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")
		s.Git(t, "push")

		// Do agent work on a feature branch.
		s.Git(t, "checkout", "-b", "feature")

		_, err := s.RunPrompt(t, ctx,
			"create a file at docs/hello.md with a paragraph about greetings. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add hello doc")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		checkpointID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		sessionMeta := testutil.WaitForSessionMetadata(t, s.Dir, checkpointID, 0, 30*time.Second)

		// Push feature branch and checkpoint refs to the bare remote.
		s.Git(t, "push", "-u", "origin", "feature")
		testutil.PushCheckpointRefs(t, s.Dir)

		// Clone the repo to a new directory (simulating a teammate).
		cloneDir := t.TempDir()
		if resolved, symErr := filepath.EvalSymlinks(cloneDir); symErr == nil {
			cloneDir = resolved
		}
		// Remove the dir because git clone wants to create it.
		require.NoError(t, os.RemoveAll(cloneDir))
		testutil.Git(t, "", "clone", bareDir, cloneDir)
		testutil.Git(t, cloneDir, "config", "user.name", "E2E Clone")
		testutil.Git(t, cloneDir, "config", "user.email", "e2e-clone@test.local")

		// Verify the checkpoint metadata ref does NOT exist locally in the clone.
		_, err = testutil.GitOutputErr(cloneDir, "rev-parse", "--verify", testutil.CheckpointVerifyRef())
		require.Error(t, err, "checkpoint metadata ref should not exist locally in clone")

		// Create the local feature branch (clone only has origin/feature as remote tracking ref).
		// Then switch back to the default branch so resume can switch to it.
		mainClone := testutil.GitOutput(t, cloneDir, "branch", "--show-current")
		testutil.Git(t, cloneDir, "checkout", "feature")
		testutil.Git(t, cloneDir, "checkout", mainClone)

		// Enable entire in the cloned repo and commit the enable files.
		entire.Enable(t, cloneDir, s.Agent.EntireAgent())
		testutil.ApplySuiteCheckpointsMode(t, cloneDir)
		testutil.CommitIfDirty(t, cloneDir, "Enable entire in clone")

		// Run resume from the clone — should fetch metadata and succeed.
		out, err := entire.Resume(cloneDir, "feature")
		require.NoError(t, err, "entire resume failed in clone: %s", out)

		current := testutil.GitOutput(t, cloneDir, "branch", "--show-current")
		assert.Equal(t, "feature", current, "should be on feature branch after resume")
		assert.Contains(t, out, "To continue", "resume output should show resume instructions")

		// Verify the checkpoint metadata ref now exists locally.
		_, err = testutil.GitOutputErr(cloneDir, "rev-parse", "--verify", testutil.CheckpointVerifyRef())
		assert.NoError(t, err, "checkpoint metadata ref should exist locally after resume")

		if restoredTranscript, ok := testutil.RestoredSessionTranscriptPath(t, cloneDir, sessionMeta); ok {
			_, err = os.Stat(restoredTranscript)
			assert.NoError(t, err, "restored session transcript should exist at %s after resume", restoredTranscript)
		}
	})
}

// TestResumeMetadataBranchAlreadyLocal: same setup as TestResumeFromClonedRepo
// but the checkpoint metadata ref already exists locally. Resume should still work
// (fetch updates local to latest).
func TestResumeMetadataBranchAlreadyLocal(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")

		// Commit files from `entire enable` so main has a clean working tree.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")

		// Do agent work on a feature branch.
		s.Git(t, "checkout", "-b", "feature")

		_, err := s.RunPrompt(t, ctx,
			"create a file at docs/hello.md with a paragraph about greetings. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add hello doc")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		// Switch back to main and resume the feature branch.
		// The metadata branch exists locally (was created during commit).
		s.Git(t, "checkout", mainBranch)

		out, err := entire.Resume(s.Dir, "feature")
		require.NoError(t, err, "entire resume failed: %s", out)

		current := testutil.GitOutput(t, s.Dir, "branch", "--show-current")
		assert.Equal(t, "feature", current, "should be on feature branch after resume")
		assert.Contains(t, out, "To continue", "resume output should show resume instructions")
	})
}
