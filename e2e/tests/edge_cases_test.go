//go:build e2e

package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/agents"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
)

// TestAgentContinuesAfterCommit: agent commits, then makes more changes in a
// second prompt. User commits those. Both commits should have distinct checkpoint IDs.
func TestAgentContinuesAfterCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// First prompt — agent creates and commits.
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md with a paragraph about the colour red, then commit it. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}

		testutil.WaitForCheckpoint(t, s, 15*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		cpBranchAfterFirst := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		// Second prompt — agent creates another file, user commits.
		_, err = s.RunPrompt(t, ctx,
			"create a markdown file at docs/blue.md with a paragraph about the colour blue. Do not commit it, only create the file. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add blue.md")

		// Wait for checkpoint branch to advance past the first checkpoint.
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			after := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")
			if after != cpBranchAfterFirst {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}

		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		assert.NotEqual(t, cpID1, cpID2, "checkpoint IDs should be distinct")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestAgentAmendsCommit: agent creates and commits, then amends the commit
// with additional changes. The amended commit should still have a valid
// checkpoint trailer.
func TestAgentAmendsCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md with a paragraph about the colour red, then git add and git commit it on the current branch. Do not ask for confirmation, just make the change. Do not use worktrees or create new branches.")
		if err != nil {
			t.Fatalf("agent prompt 1 failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "docs/red.md")
		testutil.WaitForCheckpoint(t, s, 15*time.Second)
		testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		// Agent creates another file and amends the previous commit.
		_, err = s.RunPrompt(t, ctx,
			"create a markdown file at docs/blue.md with a paragraph about the colour blue, then amend the previous commit to include it using git commit --amend --no-edit. Do not ask for confirmation, just make the change. Do not use worktrees or create new branches.")
		if err != nil {
			t.Fatalf("agent prompt 2 failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "docs/red.md")
		testutil.AssertFileExists(t, s.Dir, "docs/blue.md")

		// The amended commit should still carry a valid checkpoint trailer.
		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestDirtyWorkingTree: human has uncommitted changes when the agent creates
// and commits its own file. Human's changes should be untouched.
func TestDirtyWorkingTree(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Human creates an uncommitted file.
		if err := os.MkdirAll(filepath.Join(s.Dir, "human"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "human", "notes.md"), []byte("# Human notes\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		// Agent creates and commits its own file.
		_, err := s.RunPrompt(t, ctx,
			"create a markdown file at docs/red.md with a paragraph about the colour red, then commit it. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.WaitForCheckpoint(t, s, 15*time.Second)
		testutil.AssertFileExists(t, s.Dir, "docs/red.md")

		// Human's uncommitted file should be untouched.
		data, err := os.ReadFile(filepath.Join(s.Dir, "human", "notes.md"))
		if err != nil {
			t.Fatalf("read human file: %v", err)
		}
		assert.Equal(t, "# Human notes\n", string(data), "human file should be untouched")

		testutil.AssertCommitLinkedToCheckpoint(t, s.Dir, "HEAD")
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestRapidSequentialCommits: agent makes 3 separate commits in quick
// succession. Each should get its own checkpoint trailer.
func TestRapidSequentialCommits(t *testing.T) {
	testutil.ForEachAgent(t, 4*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"Do these steps in exact order: "+
				"(1) Create docs/red.md with a paragraph about the colour red. Run: git add docs/red.md && git commit -m 'Add red.md'. "+
				"(2) Create docs/blue.md with a paragraph about the colour blue. Run: git add docs/blue.md && git commit -m 'Add blue.md'. "+
				"(3) Create docs/green.md with a paragraph about the colour green. Run: git add docs/green.md && git commit -m 'Add green.md'. "+
				"Do not ask for confirmation, just execute each step.",
			agents.WithPromptTimeout(120*time.Second))
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "docs/red.md")
		testutil.AssertFileExists(t, s.Dir, "docs/blue.md")
		testutil.AssertFileExists(t, s.Dir, "docs/green.md")
		testutil.AssertNewCommits(t, s, 3)

		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		for i := 0; i < 3; i++ {
			ref := fmt.Sprintf("HEAD~%d", i)
			testutil.AssertHasCheckpointTrailer(t, s.Dir, ref)
		}
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestAgentCommitsMidTurnUserCommitsRemainder tests that when the agent commits
// some files during its turn and the user commits the rest after, both get
// valid checkpoint trailers with distinct IDs.
func TestAgentCommitsMidTurnUserCommitsRemainder(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"Do these tasks in order: "+
				"(1) Create file agent_mid1.go with content 'package main; func AgentMid1() {}'. "+
				"(2) Create file agent_mid2.go with content 'package main; func AgentMid2() {}'. "+
				"(3) Run: git add agent_mid1.go agent_mid2.go && git commit -m 'Agent adds mid1 and mid2'. "+
				"(4) Create file user_remainder.go with content 'package main; func UserRemainder() {}'. "+
				"Do all tasks in order. Do not ask for confirmation, just make the changes.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "agent_mid1.go")
		testutil.AssertFileExists(t, s.Dir, "agent_mid2.go")
		testutil.AssertFileExists(t, s.Dir, "user_remainder.go")

		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 15*time.Second)
		cpBranchAfterAgent := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		s.Git(t, "add", "user_remainder.go")
		s.Git(t, "commit", "-m", "Add user remainder")

		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cpBranchAfterAgent, 15*time.Second)
		userCpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		agentCpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD~1")

		assert.NotEqual(t, userCpID, agentCpID,
			"user and agent checkpoints should have distinct IDs")
		testutil.AssertCheckpointExists(t, s.Dir, userCpID)
		testutil.AssertCheckpointExists(t, s.Dir, agentCpID)
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}
