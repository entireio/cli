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

// TestPartialCommitStashNewPrompt: agent creates 3 files; user commits one,
// stashes the rest; second prompt creates 2 more files; user commits those.
// Verifies two distinct checkpoints across the stash boundary.
func TestPartialCommitStashNewPrompt(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Agent creates A, B, C.
		_, err := s.RunPrompt(t, ctx,
			"create three markdown files: docs/a.md about apples, docs/b.md about bananas, docs/c.md about cherries. Do not commit them, only create the files. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/a.md")
		testutil.AssertFileExists(t, s.Dir, "docs/b.md")
		testutil.AssertFileExists(t, s.Dir, "docs/c.md")

		// User commits only A.
		s.Git(t, "add", "docs/a.md")
		s.Git(t, "commit", "-m", "Add a.md")

		// Stash B and C.
		s.Git(t, "add", "docs/b.md", "docs/c.md")
		s.Git(t, "stash")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		// Second prompt: agent creates D, E.
		_, err = s.RunPrompt(t, ctx,
			"create two markdown files: docs/d.md about dates, docs/e.md about elderberries. Do not commit them, only create the files. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/d.md")
		testutil.AssertFileExists(t, s.Dir, "docs/e.md")

		// User commits D and E.
		s.Git(t, "add", "docs/d.md", "docs/e.md")
		s.Git(t, "commit", "-m", "Add d.md and e.md")

		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.WaitForCheckpointExists(t, s.Dir, cpID2, 30*time.Second)

		assert.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestStashSecondPromptUnstashCommitAll: agent creates 3 files; user commits
// one, stashes the rest; second prompt creates 2 more; user unstashes and
// commits all remaining files together in one commit.
func TestStashSecondPromptUnstashCommitAll(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create three markdown files: docs/a.md about apples, docs/b.md about bananas, docs/c.md about cherries. Do not commit them, only create the files. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/a.md")
		testutil.AssertFileExists(t, s.Dir, "docs/b.md")
		testutil.AssertFileExists(t, s.Dir, "docs/c.md")

		// User commits only A.
		s.Git(t, "add", "docs/a.md")
		s.Git(t, "commit", "-m", "Add a.md")

		// Stash B and C.
		s.Git(t, "add", "docs/b.md", "docs/c.md")
		s.Git(t, "stash")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		// Second prompt: agent creates D, E.
		_, err = s.RunPrompt(t, ctx,
			"create two markdown files: docs/d.md about dates, docs/e.md about elderberries. Do not commit them, only create the files. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/d.md")
		testutil.AssertFileExists(t, s.Dir, "docs/e.md")

		// Unstash B and C, commit all remaining.
		s.Git(t, "stash", "pop")
		s.Git(t, "add", "docs/")
		s.Git(t, "commit", "-m", "Add b, c, d, e")

		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.WaitForCheckpointExists(t, s.Dir, cpID2, 30*time.Second)

		assert.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestStashModificationsToTrackedFiles: agent modifies 2 existing tracked
// files; user commits one, stashes the other, pops, and commits separately.
// Verifies two distinct checkpoints for the split modifications.
func TestStashModificationsToTrackedFiles(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Create 2 tracked Go files.
		if err := os.MkdirAll(filepath.Join(s.Dir, "src"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "src", "a.go"), []byte("package src\n\n// File A placeholder.\n"), 0o644); err != nil {
			t.Fatalf("write a.go: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "src", "b.go"), []byte("package src\n\n// File B placeholder.\n"), 0o644); err != nil {
			t.Fatalf("write b.go: %v", err)
		}
		s.Git(t, "add", "src/")
		s.Git(t, "commit", "-m", "Add initial src files")

		// Agent modifies both files.
		_, err := s.RunPrompt(t, ctx,
			"Modify two existing files. In src/a.go, add a function: func Hello() string { return \"hello\" }. "+
				"In src/b.go, add a function: func World() string { return \"world\" }. "+
				"Only modify these two files, do not create new files. Do not commit. "+
				"Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		// User commits only a.go.
		s.Git(t, "add", "src/a.go")
		s.Git(t, "commit", "-m", "Update a.go")

		// Stash b.go modifications.
		s.Git(t, "stash")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		// Pop and commit b.go.
		s.Git(t, "stash", "pop")
		s.Git(t, "add", "src/b.go")
		s.Git(t, "commit", "-m", "Update b.go")

		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.WaitForCheckpointExists(t, s.Dir, cpID2, 30*time.Second)

		assert.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		// Shadow branch cleanup can lag behind condensation when carry-forward
		// creates intermediate branches, so poll instead of instant-assert.
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
