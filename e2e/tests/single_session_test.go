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

// TestHumanOnlyChangesAndCommits: human creates a file and commits without any
// agent interaction. No checkpoint should be created.
func TestHumanOnlyChangesAndCommits(t *testing.T) {
	testutil.ForEachAgent(t, 1*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Create a file and commit entirely as a human — no agent prompt.
		if err := os.MkdirAll(filepath.Join(s.Dir, "docs"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "docs", "human.md"), []byte("# Written by a human\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		s.Git(t, "add", "docs/")
		s.Git(t, "commit", "-m", "Human-only commit")

		// Give the post-commit hook time to fire (if it were going to).
		time.Sleep(5 * time.Second)

		testutil.AssertCheckpointNotAdvanced(t, s)

		trailer := testutil.GetCheckpointTrailer(t, s.Dir, "HEAD")
		assert.Empty(t, trailer, "human-only commit should not have checkpoint trailer")
	})
}

// TestSingleSessionManualCommit: one prompt creates a file, user commits manually.
func TestSingleSessionManualCommit(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md with a paragraph about the colour red. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add md file about red")

		testutil.AssertFileExists(t, s.Dir, "docs/*.md")
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.AssertCheckpointMetadataComplete(t, s.Dir, cpID)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestSingleSessionAgentCommitInTurn: one prompt creates a file and commits it.
// Expects both an initial checkpoint (post-commit) and a catchup checkpoint
// (end-of-turn) referencing the same checkpoint ID.
func TestSingleSessionAgentCommitInTurn(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md with a paragraph about the colour red, then commit it. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "docs/*.md")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.AssertCheckpointInLastN(t, s.Dir, cpID, 2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestSingleSessionSubagentCommitInTurn: one prompt with subagent creates a file and commits it.
// Expects both an initial checkpoint (post-commit) and a catchup checkpoint
// (end-of-turn) referencing the same checkpoint ID.
func TestSingleSessionSubagentCommitInTurn(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if s.Agent.Name() == "copilot-cli" {
			t.Skip("copilot-cli may run prompt-mode subagents in the background while the foreground process returns and fires agent-stop/session-end hooks first; when the later subagent commit happens, git hooks see no active session, so this same-turn subagent-commit assertion is not stable for Copilot")
		}

		_, err := s.RunPrompt(t, ctx,
			"use a subagent: create a markdown file at docs/red.md with a paragraph about the colour red, then commit it. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "docs/*.md")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.AssertCheckpointInLastN(t, s.Dir, cpID, 2)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
