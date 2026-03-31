//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
)

// TestMultiSessionManualCommit: two prompts create files, user commits manually.
func TestMultiSessionManualCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx, "create a markdown file at docs/red.md with a paragraph about the colour red. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}

		_, err = s.RunPrompt(t, ctx, "create a markdown file at docs/blue.md with a paragraph about the colour blue. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add md files about red and blue")

		testutil.AssertFileExists(t, s.Dir, "docs/*.md")
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestMultiSessionSequential: two prompts each commit separately, distinct checkpoints.
func TestMultiSessionSequential(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md about the colour red, then git add and git commit it with a short message. Do not ask for confirmation, just make the change. Do not include any trailers or metadata in the commit message. Do not use worktrees.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}

		_, err = s.RunPrompt(t, ctx,
			"create a markdown file at docs/blue.md about the colour blue, then git add and git commit it with a short message. Do not ask for confirmation, just make the change. Do not include any trailers or metadata in the commit message. Do not use worktrees.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "docs/*.md")
		testutil.AssertNewCommits(t, s, 2)

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD~1")
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)

		cpIDs := testutil.CheckpointIDs(t, s.Dir)
		for _, id := range cpIDs {
			testutil.AssertCheckpointHasSingleSession(t, s.Dir, id)
		}
		testutil.AssertDistinctSessions(t, s.Dir, cpIDs)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
