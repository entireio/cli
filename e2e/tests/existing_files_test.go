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

// TestModifyExistingTrackedFile: agent modifies an existing tracked file
// (not a new file), user commits. Checkpoint should be created.
func TestModifyExistingTrackedFile(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Create a tracked file.
		if err := os.MkdirAll(filepath.Join(s.Dir, "src"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "src", "config.go"), []byte("package src\n\n// Config placeholder.\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		s.Git(t, "add", "src/")
		s.Git(t, "commit", "-m", "Add initial config.go")

		// Agent modifies the existing file.
		_, err := s.RunPrompt(t, ctx,
			"modify src/config.go to add a function GetPort() int that returns 8080. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Update config.go")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestMixedNewAndModifiedFiles: agent modifies an existing file AND creates
// new files in the same session. User commits the modification first, then the
// new files. Two distinct checkpoints.
func TestMixedNewAndModifiedFiles(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Create a tracked file.
		if err := os.MkdirAll(filepath.Join(s.Dir, "src"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		s.Git(t, "add", "src/")
		s.Git(t, "commit", "-m", "Add initial main.go")

		// Agent modifies main.go AND creates new files.
		_, err := s.RunPrompt(t, ctx,
			"modify src/main.go to add a main function, and also create exactly two new files: src/utils.go with a helper function and src/types.go with a User type definition. Put them directly in the src/ directory, not in any subdirectory. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		// Commit the modification first.
		s.Git(t, "add", "src/main.go")
		s.Git(t, "commit", "-m", "Update main.go")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		// Commit remaining files (use "." to catch any extras the agent created).
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add utils.go and types.go")

		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.WaitForCheckpointExists(t, s.Dir, cpID2, 30*time.Second)

		assert.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestInteractiveContentOverlapRevertNewFile: agent creates a file, user replaces its
// content entirely with different text and commits while the session is still
// idle (not ended). The content-aware overlap detection should prevent a
// checkpoint trailer (content mismatch on new file). The shadow branch
// correctly persists because the session is still active and no condensation
// occurred.
func TestInteractiveContentOverlapRevertNewFile(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := s.Agent.PromptPattern()

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skipf("agent %s does not support interactive mode", s.Agent.Name())
		}

		s.WaitFor(t, session, prompt, 30*time.Second)

		s.Send(t, session, "create a markdown file at docs/red.md with a paragraph about the colour red. Don't commit, I want to make more changes.")
		s.WaitFor(t, session, prompt, 60*time.Second)
		testutil.WaitForFileExists(t, s.Dir, "docs/red.md", 30*time.Second)

		// Wait for session to transition from ACTIVE to IDLE. The prompt
		// pattern may appear before the turn-end hook completes (race between
		// TUI rendering and hook execution, especially with OpenCode).
		testutil.WaitForSessionIdle(t, s.Dir, 15*time.Second)

		// Session is now idle (turn ended, waiting for next prompt).
		// User replaces the content entirely.
		if err := os.WriteFile(filepath.Join(s.Dir, "docs", "red.md"), []byte("# Completely different content\n\nNothing about red here.\n"), 0o644); err != nil {
			t.Fatalf("overwrite file: %v", err)
		}

		s.Git(t, "add", "docs/red.md")
		s.Git(t, "commit", "-m", "Replace red.md content")

		// Give post-commit hook time to fire.
		time.Sleep(5 * time.Second)

		testutil.AssertNoCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointNotAdvanced(t, s)
		// Shadow branch correctly persists: session is idle and no
		// condensation occurred (content mismatch on new file).
		testutil.AssertHasShadowBranches(t, s.Dir)
	})
}

// TestModifiedFileAlwaysGetsCheckpoint: agent modifies an existing tracked
// file, user writes completely different content and commits. A checkpoint
// should STILL be created because content-aware overlap detection only
// applies to new files, not modifications to existing tracked files.
func TestModifiedFileAlwaysGetsCheckpoint(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Create a tracked file.
		if err := os.MkdirAll(filepath.Join(s.Dir, "src"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "src", "config.go"), []byte("package src\n\n// Config placeholder.\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		s.Git(t, "add", "src/")
		s.Git(t, "commit", "-m", "Add initial config.go")

		// Agent modifies the file.
		_, err := s.RunPrompt(t, ctx,
			"modify src/config.go to add a function GetPort() int that returns 8080. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		// User writes completely different content (ignoring agent's changes).
		if err := os.WriteFile(filepath.Join(s.Dir, "src", "config.go"), []byte("package src\n\n// User rewrote this entirely.\nfunc GetHost() string { return \"localhost\" }\n"), 0o644); err != nil {
			t.Fatalf("overwrite file: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Rewrite config.go")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
