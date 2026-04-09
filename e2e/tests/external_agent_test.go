//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
)

// TestExternalAgentSingleSessionManualCommit: external agent creates a file,
// user commits manually. Validates the full external agent protocol flow:
// discovery via PATH, hook firing through binary subcommands, transcript
// handling, and checkpoint creation.
func TestExternalAgentSingleSessionManualCommit(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"Create a file at docs/hello.md with the content 'Hello World'. Do not ask for confirmation.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "docs/hello.md")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add hello file via external agent")

		testutil.AssertNewCommits(t, s, 1)
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.AssertCheckpointMetadataComplete(t, s.Dir, cpID)
		testutil.AssertCheckpointHasSingleSession(t, s.Dir, cpID)
		testutil.AssertCheckpointFilesTouchedContains(t, s.Dir, cpID, "docs/hello.md")
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}

// TestExternalAgentMultipleTurnsManualCommit: external agent handles two
// sequential prompts, user commits once. Both prompts should be captured
// in a single checkpoint.
func TestExternalAgentMultipleTurnsManualCommit(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if !s.IsExternalAgent() {
			t.Skip("skipping external agent test for non-external agent")
		}

		_, err := s.RunPrompt(t, ctx,
			"create a file called src/alpha.txt with a short paragraph. Do not ask for confirmation or approval, just make the change.")
		if err != nil {
			t.Fatalf("first prompt failed: %v", err)
		}

		_, err = s.RunPrompt(t, ctx,
			"create a file called src/beta.txt with a short paragraph. Do not ask for confirmation or approval, just make the change.")
		if err != nil {
			t.Fatalf("second prompt failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "src/alpha.txt")
		testutil.AssertFileExists(t, s.Dir, "src/beta.txt")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add alpha and beta via external agent")

		testutil.AssertNewCommits(t, s, 1)
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)
		testutil.AssertCheckpointMetadataComplete(t, s.Dir, cpID)
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)

		// Both files should appear in files_touched
		testutil.AssertCheckpointFilesTouchedContains(t, s.Dir, cpID, "src/alpha.txt")
		testutil.AssertCheckpointFilesTouchedContains(t, s.Dir, cpID, "src/beta.txt")
	})
}

// TestExternalAgentDeepCheckpointValidation: verifies transcript content,
// content hash, and prompt text are correctly captured through the external
// agent protocol.
func TestExternalAgentDeepCheckpointValidation(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a file called notes/deep.md with a paragraph about deep validation. Do not ask for confirmation or approval, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		testutil.AssertFileExists(t, s.Dir, "notes/deep.md")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Deep checkpoint validation test")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		testutil.ValidateCheckpointDeep(t, s.Dir, testutil.DeepCheckpointValidation{
			CheckpointID:    cpID,
			Strategy:        "manual-commit",
			FilesTouched:    []string{"notes/deep.md"},
			ExpectedPrompts: []string{"create a file called notes/deep.md"},
		})
	})
}

// TestExternalAgentSessionMetadata: verifies that checkpoint session metadata
// correctly identifies the external agent type.
func TestExternalAgentSessionMetadata(t *testing.T) {
	testutil.ForEachAgent(t, 2*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a file called meta/test.md with a short paragraph. Do not ask for confirmation or approval, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Metadata test commit")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		// Verify session metadata has a non-empty agent field
		sm := testutil.ReadSessionMetadata(t, s.Dir, cpID, 0)
		assert.NotEmpty(t, sm.Agent, "session metadata should have agent field set")
		assert.NotEmpty(t, sm.SessionID, "session metadata should have session_id")
	})
}
