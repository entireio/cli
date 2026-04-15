//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
)

func TestFactoryTaskHooksDoNotFail(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if s.Agent.Name() != "factoryai-droid" {
			t.Skip("factory-only regression test")
		}

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skip("factoryai-droid does not support interactive mode")
		}

		s.WaitFor(t, session, s.Agent.PromptPattern(), 30*time.Second)
		s.Send(t, session,
			"Can you run a Worker that inspects this code and comes up with a short summary about what it is about? Have the Worker write that summary to docs/factory-hook-check.md as one short paragraph followed by exactly 3 bullet points. Do not create or edit the file in the main agent process. Only the Worker should write the file. Do not commit. Do not ask for confirmation.")
		s.WaitFor(t, session, s.Agent.PromptPattern(), 90*time.Second)

		testutil.WaitForFileExists(t, s.Dir, "docs/factory-hook-check.md", 10*time.Second)
		testutil.AssertConsoleLogDoesNotContain(t, s,
			"tool_use_id is required",
			"failed to parse hook event",
			"postToolHookInputRaw.tool_response",
		)
	})
}
