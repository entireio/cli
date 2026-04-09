//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
)

func TestInteractiveMultiStep(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := s.Agent.PromptPattern()

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skipf("agent %s does not support interactive mode", s.Agent.Name())
		}

		s.WaitFor(t, session, prompt, 30*time.Second)

		s.Send(t, session, "create a markdown file at docs/red.md with a paragraph about the colour red. Do not ask for confirmation, just make the change.")
		s.WaitFor(t, session, prompt, 60*time.Second)
		testutil.WaitForFileExists(t, s.Dir, "docs/*.md", 30*time.Second)

		s.Send(t, session, "now commit it")
		s.WaitFor(t, session, prompt, 60*time.Second)
		testutil.AssertNewCommitsWithTimeout(t, s, 1, 60*time.Second)

		// Wait for the turn-end hook (including finalize) to complete before
		// reading the checkpoint branch. The finalize step writes a second
		// commit to entire/checkpoints/v1 concurrently with the test, and
		// reading the branch mid-update can see a broken ref.
		testutil.WaitForSessionIdle(t, s.Dir, 15*time.Second)
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCommitLinkedToCheckpoint(t, s.Dir, "HEAD")
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
