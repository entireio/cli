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
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCommitLinkedToCheckpoint(t, s.Dir, "HEAD")
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
