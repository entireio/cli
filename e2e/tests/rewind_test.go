//go:build e2e

package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRewindPreCommit: agent creates two files in sequence (no user commit),
// then rewinds to the point after the first file was created.
func TestRewindPreCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Agent creates file A.
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md with a paragraph about the colour red. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/red.md")

		pointsAfterA := entire.RewindList(t, s.Dir)
		require.NotEmpty(t, pointsAfterA, "expected at least 1 rewind point after file A")

		// Agent creates file B.
		_, err = s.RunPrompt(t, ctx,
			"create a markdown file at docs/blue.md with a paragraph about the colour blue. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/blue.md")

		pointsAfterB := entire.RewindList(t, s.Dir)
		require.Greater(t, len(pointsAfterB), len(pointsAfterA),
			"expected more rewind points after file B")

		// Rewind to the first rewind point (after file A was created).
		rewindID := pointsAfterA[0].ID
		err = entire.Rewind(t, s.Dir, rewindID)
		require.NoError(t, err, "rewind to %s should succeed", rewindID)

		// File A should still exist, file B should be gone.
		testutil.AssertFileExists(t, s.Dir, "docs/red.md")
		_, statErr := os.Stat(filepath.Join(s.Dir, "docs", "blue.md"))
		assert.True(t, os.IsNotExist(statErr),
			"docs/blue.md should not exist after rewind, stat error: %v", statErr)
	})
}

// TestRewindAfterCommit: pre-commit shadow branch rewind points should become
// invalid after a user commit. Rewinding to an old shadow branch ID should fail.
func TestRewindAfterCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Agent creates a file.
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md with a paragraph about the colour red. Do not commit the file. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/red.md")

		// Get pre-commit rewind points (shadow branch).
		pointsBefore := entire.RewindList(t, s.Dir)
		require.NotEmpty(t, pointsBefore, "expected rewind points before commit")

		// Find a non-logs-only point (shadow branch ID).
		var shadowPoint *entire.RewindPoint
		for i := range pointsBefore {
			if !pointsBefore[i].IsLogsOnly {
				shadowPoint = &pointsBefore[i]
				break
			}
		}
		require.NotNil(t, shadowPoint, "expected at least one shadow branch rewind point")
		oldID := shadowPoint.ID

		// User commits the file.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add red.md")

		testutil.WaitForCheckpoint(t, s, 15*time.Second)

		// Get rewind points after commit — old shadow IDs should be gone.
		pointsAfter := entire.RewindList(t, s.Dir)

		// Verify the old shadow branch ID is no longer listed.
		found := false
		for _, p := range pointsAfter {
			if p.ID == oldID && !p.IsLogsOnly {
				found = true
				break
			}
		}
		assert.False(t, found, "old shadow branch rewind point %s should no longer be listed", oldID)

		// Attempting to rewind to the old shadow branch ID should fail.
		err = entire.Rewind(t, s.Dir, oldID)
		assert.Error(t, err, "rewind to old shadow branch ID should fail after commit")

		// Working directory should be unchanged — file still committed.
		testutil.AssertFileExists(t, s.Dir, "docs/red.md")
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestRewindMultipleFiles: agent creates files across two prompts, rewind
// drops only the second prompt's changes.
func TestRewindMultipleFiles(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// First prompt — create file A. Use non-colour filename to avoid
		// Gemini "helpfully" creating the entire rainbow.
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/readme.md with a paragraph about this project. Do not create any other files. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/readme.md")

		// Capture rewind points after first prompt.
		pointsAfterA := entire.RewindList(t, s.Dir)
		require.NotEmpty(t, pointsAfterA, "expected rewind points after first prompt")

		// Second prompt — create file B (unrelated topic).
		_, err = s.RunPrompt(t, ctx,
			"create a markdown file at docs/changelog.md with a paragraph about recent changes. Do not create any other files. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/changelog.md")

		// Rewind to a point from after the first prompt.
		rewindID := pointsAfterA[0].ID
		err = entire.Rewind(t, s.Dir, rewindID)
		require.NoError(t, err, "rewind to %s should succeed", rewindID)

		// File A should remain, file B should be gone.
		testutil.AssertFileExists(t, s.Dir, "docs/readme.md")
		_, statErr := os.Stat(filepath.Join(s.Dir, "docs", "changelog.md"))
		assert.True(t, os.IsNotExist(statErr),
			"docs/changelog.md should not exist after rewind, stat error: %v", statErr)
	})
}

// TestRewindSquashMergeMultipleCheckpoints: when a squash merge commit contains
// multiple Entire-Checkpoint trailers, `entire rewind --list` should show a
// single logs-only point using the latest checkpoint. Logs-only restore should
// succeed and return the most recent session's transcript.
func TestRewindSquashMergeMultipleCheckpoints(t *testing.T) {
	testutil.ForEachAgent(t, 5*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")

		// Commit files from `entire enable` so main has a clean working tree.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")

		// Create feature branch with two agent-assisted commits.
		s.Git(t, "checkout", "-b", "feature")

		_, err := s.RunPrompt(t, ctx,
			"create a file at docs/red.md with a paragraph about the colour red. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("prompt 1 failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add red doc")
		testutil.WaitForCheckpoint(t, s, 15*time.Second)
		cp1Ref := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		_, err = s.RunPrompt(t, ctx,
			"create a file at docs/blue.md with a paragraph about the colour blue. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("prompt 2 failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add blue doc")
		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cp1Ref, 15*time.Second)

		// Record checkpoint IDs from both feature branch commits.
		cpID1 := testutil.GetCheckpointTrailer(t, s.Dir, "HEAD~1")
		cpID2 := testutil.GetCheckpointTrailer(t, s.Dir, "HEAD")
		require.NotEmpty(t, cpID1, "first commit should have checkpoint trailer")
		require.NotEmpty(t, cpID2, "second commit should have checkpoint trailer")
		require.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")

		// Squash merge onto main with both checkpoint trailers in the message.
		s.Git(t, "checkout", mainBranch)
		s.Git(t, "merge", "--squash", "feature")
		squashMsg := fmt.Sprintf(
			"Squash merge feature (#1)\n\n* Add red doc\n\nEntire-Checkpoint: %s\n\n* Add blue doc\n\nEntire-Checkpoint: %s",
			cpID1, cpID2,
		)
		s.Git(t, "commit", "-m", squashMsg)
		squashHash := testutil.GitOutput(t, s.Dir, "rev-parse", "HEAD")

		// Rewind list should show exactly one logs-only point for the squash commit,
		// using the latest checkpoint (cpID2).
		points := entire.RewindList(t, s.Dir)

		var squashPoints []entire.RewindPoint
		for _, p := range points {
			if p.ID == squashHash && p.IsLogsOnly {
				squashPoints = append(squashPoints, p)
			}
		}
		require.Len(t, squashPoints, 1,
			"expected exactly 1 logs-only rewind point for squash commit")
		assert.Equal(t, cpID2, squashPoints[0].CondensationID,
			"squash merge rewind point should use the latest checkpoint ID")

		// Logs-only restore should succeed.
		err = entire.RewindLogsOnly(t, s.Dir, squashHash)
		require.NoError(t, err, "logs-only rewind of squash merge commit should succeed")
	})
}
