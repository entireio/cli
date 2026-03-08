//go:build e2e

package tests

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResumeFromFeatureBranch: agent creates a file on a feature branch and
// user commits, then switches back to main and runs `entire resume feature`.
// Verifies the branch is switched and the session is restored.
func TestResumeFromFeatureBranch(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")

		// Commit files from `entire enable` so main has a clean working tree
		// for branch switching (mirrors a real repo where .gitignore is tracked).
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
		testutil.WaitForCheckpoint(t, s, 15*time.Second)

		// Switch back to main and resume the feature branch.
		s.Git(t, "checkout", mainBranch)

		out, err := entire.Resume(s.Dir, "feature")
		require.NoError(t, err, "entire resume failed: %s", out)

		current := testutil.GitOutput(t, s.Dir, "branch", "--show-current")
		assert.Equal(t, "feature", current, "should be on feature branch after resume")
		assert.Contains(t, out, "To continue", "resume output should show resume instructions")
	})
}

// TestResumeSquashMergeMultipleCheckpoints: two agent prompts on a feature
// branch each get their own commit and checkpoint. The feature branch is
// squash-merged to main and `entire resume` should restore only the latest
// checkpoint (by CreatedAt), skipping older ones. Tests both squash merge message formats:
//
//   - GitHub format: trailers appear at the top level in the commit body
//     (e.g. "* Add red doc\n\nEntire-Checkpoint: aaa\n\n* Add blue doc\n\nEntire-Checkpoint: bbb")
//   - git CLI format: `git merge --squash` nests original commit messages
//     (including trailers) indented with 4 spaces inside the squash message
//
// Both formats are tested in a single function to share the expensive agent
// prompts. Main is reset between format tests.
func TestResumeSquashMergeMultipleCheckpoints(t *testing.T) {
	testutil.ForEachAgent(t, 5*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")

		// Commit files from `entire enable` so main has a clean working tree
		// for branch switching and squash merging.
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

		// Save main HEAD so we can reset between format tests.
		s.Git(t, "checkout", mainBranch)
		mainHead := testutil.GitOutput(t, s.Dir, "rev-parse", "HEAD")

		// --- Format 1: GitHub squash merge ---
		// GitHub puts each original commit message (including trailers) as
		// top-level bullet points in the squash commit body.
		s.Git(t, "merge", "--squash", "feature")
		githubMsg := fmt.Sprintf(
			"Squash merge feature (#1)\n\n* Add red doc\n\nEntire-Checkpoint: %s\n\n* Add blue doc\n\nEntire-Checkpoint: %s",
			cpID1, cpID2,
		)
		s.Git(t, "commit", "-m", githubMsg)

		out, err := entire.Resume(s.Dir, mainBranch)
		require.NoError(t, err, "github format: entire resume failed: %s", out)
		assert.Contains(t, out, "older checkpoints skipped",
			"github format: squash merge should skip older checkpoints")

		// Reset main to before the squash merge for the next format test.
		s.Git(t, "reset", "--hard", mainHead)

		// --- Format 2: git merge --squash ---
		// The git CLI nests original commit messages (with trailers) indented
		// by 4 spaces inside "Squashed commit of the following:".
		s.Git(t, "merge", "--squash", "feature")

		// Sanity-check: the auto-generated SQUASH_MSG should contain both trailers.
		squashMsgBytes, err := os.ReadFile(filepath.Join(s.Dir, ".git", "SQUASH_MSG"))
		require.NoError(t, err, "read .git/SQUASH_MSG")
		squashMsgStr := string(squashMsgBytes)
		require.Contains(t, squashMsgStr, cpID1,
			"git SQUASH_MSG should contain first checkpoint ID")
		require.Contains(t, squashMsgStr, cpID2,
			"git SQUASH_MSG should contain second checkpoint ID")

		// Commit using the git-generated squash message directly (not via -m).
		// GIT_EDITOR=true prevents git from opening an editor while letting
		// it use .git/SQUASH_MSG natively with all hooks running.
		commitCmd := exec.Command("git", "commit")
		commitCmd.Dir = s.Dir
		commitCmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=0", "GIT_EDITOR=true")
		commitOut, commitErr := commitCmd.CombinedOutput()
		fmt.Fprintf(s.ConsoleLog, "> git commit (GIT_EDITOR=true)\n%s\n", commitOut)
		require.NoError(t, commitErr, "git commit with squash message failed: %s", commitOut)

		out, err = entire.Resume(s.Dir, mainBranch)
		require.NoError(t, err, "git-cli format: entire resume failed: %s", out)
		assert.Contains(t, out, "older checkpoints skipped",
			"git-cli format: squash merge should skip older checkpoints")
	})
}

// TestResumeNoCheckpointOnBranch: resume on a feature branch that has only
// human commits (no agent interaction). Should switch to the branch and exit
// cleanly with an informational message, not an error.
func TestResumeNoCheckpointOnBranch(t *testing.T) {
	testutil.ForEachAgent(t, 1*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")

		// Commit files from `entire enable` so main has a clean working tree.
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")

		// Create a feature branch with only human commits.
		s.Git(t, "checkout", "-b", "no-checkpoint")
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir, "docs"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(s.Dir, "docs", "human.md"),
			[]byte("# Written by a human\n"), 0o644,
		))
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Human-only commit")

		// Switch back to main and try to resume the feature branch.
		s.Git(t, "checkout", mainBranch)

		out, err := entire.Resume(s.Dir, "no-checkpoint")
		require.NoError(t, err, "resume should not error for missing checkpoints: %s", out)

		assert.Contains(t, out, "No Entire checkpoint found",
			"should inform user no checkpoint exists on branch")
	})
}

// TestResumeOlderCheckpointWithNewerCommits: agent creates a file on a feature
// branch and user commits (checkpoint created), then user adds a human-only
// commit on top. `entire resume --force` should still find and restore the
// older checkpoint despite the newer commits without checkpoints.
func TestResumeOlderCheckpointWithNewerCommits(t *testing.T) {
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
		testutil.WaitForCheckpoint(t, s, 15*time.Second)

		// Add a human-only commit on top (no agent involvement, no checkpoint).
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir, "notes"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(s.Dir, "notes", "todo.md"),
			[]byte("# TODO\n- something\n"), 0o644,
		))
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Human-only follow-up")

		// Switch back to main and resume (--force is always passed by
		// entire.Resume, which bypasses the "older checkpoint" confirmation).
		s.Git(t, "checkout", mainBranch)

		out, err := entire.Resume(s.Dir, "feature")
		require.NoError(t, err, "entire resume failed: %s", out)

		current := testutil.GitOutput(t, s.Dir, "branch", "--show-current")
		assert.Equal(t, "feature", current, "should be on feature branch after resume")
		assert.Contains(t, out, "To continue", "should restore the older checkpoint session")
	})
}
