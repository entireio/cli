//go:build e2e

package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
)

// TestEndedSessionUserCommitsAfterExit tests that after an agent session ends
// naturally, user commits still get checkpoint trailers.
func TestEndedSessionUserCommitsAfterExit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"Create three files: ended_a.go with 'package main; func EndedA() {}', "+
				"ended_b.go with 'package main; func EndedB() {}', "+
				"ended_c.go with 'package main; func EndedC() {}'. "+
				"Create all three files, nothing else. Do not commit. "+
				"Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "ended_a.go")
		testutil.AssertFileExists(t, s.Dir, "ended_b.go")
		testutil.AssertFileExists(t, s.Dir, "ended_c.go")

		s.Git(t, "add", "ended_a.go", "ended_b.go")
		s.Git(t, "commit", "-m", "Add ended files A and B")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		cpBranchAfterFirst := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		s.Git(t, "add", "ended_c.go")
		s.Git(t, "commit", "-m", "Add ended file C")
		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cpBranchAfterFirst, 30*time.Second)
		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		assert.NotEqual(t, cpID1, cpID2, "each commit should have its own checkpoint ID")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestSessionDepletedManualEditNoCheckpoint tests that once all session files
// are committed, subsequent manual edits do NOT get checkpoint trailers.
func TestSessionDepletedManualEditNoCheckpoint(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"Create a file called depleted.go with content 'package main; func Depleted() {}'. "+
				"Create only this file. Do not commit. "+
				"Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "depleted.go")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add depleted.go")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)

		cpBranchAfterAgent := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		os.WriteFile(filepath.Join(s.Dir, "depleted.go"),
			[]byte("package main\n\n// Manual user edit\nfunc Depleted() { return }\n"), 0o644)

		s.Git(t, "add", "depleted.go")
		s.Git(t, "commit", "-m", "Manual edit to depleted.go")

		time.Sleep(5 * time.Second)
		cpBranchAfterManual := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")
		assert.Equal(t, cpBranchAfterAgent, cpBranchAfterManual,
			"manual edit after session depletion should not advance checkpoint branch")
		testutil.AssertNoCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestTrailerRemovalSkipsCondensation tests that when a user removes the
// Entire-Checkpoint trailer from the commit message, no condensation happens.
func TestTrailerRemovalSkipsCondensation(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"Create a file called trailer_test.go with content 'package main; func TrailerTest() {}'. "+
				"Create only this file. Do not commit. "+
				"Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "trailer_test.go")

		s.Git(t, "add", ".")
		s.Git(t, "-c", "core.hooksPath=/dev/null", "commit", "-m", "Add trailer_test (no checkpoint)")

		testutil.AssertNoCheckpointTrailer(t, s.Dir, "HEAD")

		time.Sleep(5 * time.Second)
		testutil.AssertCheckpointNotAdvanced(t, s)
		// Shadow branch legitimately persists: hooks were bypassed so
		// post-commit cleanup never ran.
	})
}
