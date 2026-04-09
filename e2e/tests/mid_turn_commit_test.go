//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
)

// TestMidTurnCommit_DifferentFilesThanPreviousTurn: Turn 1 creates file A
// (tracked via Stop checkpoint), Turn 2 creates+commits different files B,C.
// Validates that the mid-turn commit gets a checkpoint on entire/checkpoints/v1
// even though the committed files (B,C) don't overlap with Turn 1's tracked
// files (A).
//
// This is a regression test for the bug where shouldCondenseWithOverlapCheck
// skipped condensation for ACTIVE sessions because filesTouchedBefore (from
// Turn 1) didn't overlap with the committed files (from Turn 2).
//
// Category: agent-validation — this test validates hook behavior that varies
// by agent (commit flow, hook timing). It should run for every new agent
// integration, not just as a one-time regression check.
// TODO: when we split E2E tests into "always run" vs "new agent validation",
// this belongs in the "new agent validation" category.
func TestMidTurnCommit_DifferentFilesThanPreviousTurn(t *testing.T) {
	testutil.ForEachAgent(t, 4*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Turn 1: Agent creates a file (tracked as touched). No commit.
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/turn1.md with a paragraph about apples. Do not commit the file. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 1 (turn 1) failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/turn1.md")

		// Turn 2: Agent creates DIFFERENT files and commits them mid-turn.
		// The committed files (turn2a.md, turn2b.md) do NOT overlap with
		// Turn 1's tracked files (turn1.md).
		_, err = s.RunPrompt(t, ctx,
			"create two markdown files: docs/turn2a.md about bananas and docs/turn2b.md about cherries. Then git add and git commit both files with a short message. Do not commit any other files. Do not ask for confirmation, just make the changes. Do not add Co-authored-by or Signed-off-by trailers. Do not use worktrees.")
		if err != nil {
			t.Fatalf("agent prompt 2 (turn 2) failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/turn2a.md")
		testutil.AssertFileExists(t, s.Dir, "docs/turn2b.md")

		// The agent committed mid-turn, so there should be at least 1 new commit.
		testutil.AssertNewCommits(t, s, 1)

		// CRITICAL: The mid-turn commit should have triggered condensation
		// to entire/checkpoints/v1, even though committed files differ from
		// Turn 1's tracked files. This is the regression assertion.
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
