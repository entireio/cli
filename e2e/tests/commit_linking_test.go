//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
)

// TestInteractiveAgentCommitInSameTurn: in an interactive session the agent
// creates a file and commits it within the same turn. The commit happens
// before that turn's Stop, so no turn end has recorded the file in session
// state yet; the mid-turn commit must still link the session and write its
// checkpoint.
func TestInteractiveAgentCommitInSameTurn(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := s.Agent.PromptPattern()

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skipf("agent %s does not support interactive mode", s.Agent.Name())
		}

		s.WaitFor(t, session, prompt, 30*time.Second)

		s.Send(t, session, "create a file called hello.txt containing 'hello world', then commit it. Do not ask for confirmation.")
		s.WaitFor(t, session, prompt, 90*time.Second)
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.WaitForCheckpointExists(t, s.Dir, cpID, 30*time.Second)
	})
}

// TestInteractiveSecondCommitSameFileDistinctCheckpoint: two prompts in the
// same interactive session. The agent creates a file and commits it, then in a
// later turn modifies the same file and makes a new commit. Each commit must
// carry its own checkpoint: the second must not reuse the first's ID.
func TestInteractiveSecondCommitSameFileDistinctCheckpoint(t *testing.T) {
	testutil.ForEachAgent(t, 4*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := s.Agent.PromptPattern()

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skipf("agent %s does not support interactive mode", s.Agent.Name())
		}

		s.WaitFor(t, session, prompt, 30*time.Second)

		// First prompt: create the file and commit it.
		s.Send(t, session, "create a file called poem.txt with a short poem about coding, then commit it. Do not ask for confirmation.")
		s.WaitFor(t, session, prompt, 60*time.Second)
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID1 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		// Second prompt: modify the same file and commit again.
		s.Send(t, session, "add another stanza to poem.txt about debugging, then create a NEW commit (do not amend). Do not ask for confirmation.")
		s.WaitFor(t, session, prompt, 90*time.Second)
		testutil.AssertNewCommitsWithTimeout(t, s, 2, 60*time.Second)

		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.WaitForCheckpointExists(t, s.Dir, cpID2, 30*time.Second)

		assert.NotEqual(t, cpID1, cpID2, "the second commit must get its own checkpoint")
		testutil.AssertCheckpointExists(t, s.Dir, cpID1)
		testutil.AssertCheckpointExists(t, s.Dir, cpID2)
	})
}
