//go:build e2e

package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/agents"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
)

// TestUserSplitsAgentChanges: agent creates 4 files in one prompt; user
// commits them in two separate batches. Each batch gets its own checkpoint.
func TestUserSplitsAgentChanges(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create four markdown files: docs/a.md about apples, docs/b.md about bananas, docs/c.md about cherries, docs/d.md about dates. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/a.md")
		testutil.AssertFileExists(t, s.Dir, "docs/b.md")
		testutil.AssertFileExists(t, s.Dir, "docs/c.md")
		testutil.AssertFileExists(t, s.Dir, "docs/d.md")

		// User commits A + B.
		s.Git(t, "add", "docs/a.md", "docs/b.md")
		s.Git(t, "commit", "-m", "Add a.md and b.md")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		cpBranch1 := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		// Commit everything remaining (c.md + d.md + any extra files the agent might have created).
		s.Git(t, "add", "-A")
		s.Git(t, "commit", "-m", "Commit remaining changes (including c.md and d.md)")

		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cpBranch1, 30*time.Second)
		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		assert.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestPartialStaging: agent modifies a file, user commits. Second prompt
// modifies the same file again, user commits. Two distinct checkpoints both
// reference the same file.
func TestPartialStaging(t *testing.T) {
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

		// First prompt: agent modifies the file.
		_, err := s.RunPrompt(t, ctx,
			"modify src/main.go to add a main function that prints \"hello world\". Do not ask for confirmation, just make the change.",
			agents.WithPromptTimeout(2*time.Minute))
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}

		s.Git(t, "add", "src/main.go")
		s.Git(t, "commit", "-m", "Add hello world")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		cpBranch1 := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		// Second prompt: agent modifies the same file again.
		_, err = s.RunPrompt(t, ctx,
			"modify src/main.go to also print \"goodbye world\" after the hello line. Do not ask for confirmation, just make the change.",
			agents.WithPromptTimeout(2*time.Minute))
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}

		s.Git(t, "add", "-A")
		s.Git(t, "commit", "-m", "Add goodbye world")

		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cpBranch1, 30*time.Second)
		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		assert.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestSplitModificationsToExistingFiles: agent modifies 3 existing tracked
// files; user commits each one separately. Three distinct checkpoints.
func TestSplitModificationsToExistingFiles(t *testing.T) {
	testutil.ForEachAgent(t, 4*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Create 3 tracked files.
		if err := os.MkdirAll(filepath.Join(s.Dir, "src"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for _, name := range []string{"model.go", "view.go", "controller.go"} {
			content := "package src\n\n// " + name + " placeholder.\n"
			if err := os.WriteFile(filepath.Join(s.Dir, "src", name), []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		s.Git(t, "add", "src/")
		s.Git(t, "commit", "-m", "Add MVC skeleton")

		// Agent modifies all 3 files.
		_, err := s.RunPrompt(t, ctx,
			"modify these three files: src/model.go should define a User struct with Name and Email fields, src/view.go should add a RenderUser function, src/controller.go should add a HandleUser function. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		// Commit model.go.
		s.Git(t, "add", "src/model.go")
		s.Git(t, "commit", "-m", "Update model.go")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		cpBranch1 := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		// Commit view.go.
		s.Git(t, "add", "src/view.go")
		s.Git(t, "commit", "-m", "Update view.go")

		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cpBranch1, 30*time.Second)
		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		cpBranch2 := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		// Commit everything remaining (controller.go + any extra files the agent might have created).
		s.Git(t, "add", "-A")
		s.Git(t, "commit", "-m", "Commit remaining changes")

		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cpBranch2, 30*time.Second)
		cpID3 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		// All three checkpoints should be distinct and valid.
		assert.NotEqual(t, cpID1, cpID2, "checkpoint 1 and 2 should be distinct")
		assert.NotEqual(t, cpID2, cpID3, "checkpoint 2 and 3 should be distinct")
		assert.NotEqual(t, cpID1, cpID3, "checkpoint 1 and 3 should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.AssertCheckpointExists(t, s.Dir, cpID3)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
