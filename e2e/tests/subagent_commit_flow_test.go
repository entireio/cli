//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
)

// TestSubagentCommitFlow: agent creates a file via subagent, user commits.
// Validates the full checkpoint flow: trailer, metadata (cli_version, strategy,
// sessions), session metadata (agent field), and checkpoint existence.
func TestSubagentCommitFlow(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"use a subagent: create a markdown file at docs/red.md with a paragraph about the colour red. Do not commit the file. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/red.md")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add red.md via subagent")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.AssertCheckpointAdvanced(t, s)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		testutil.AssertCheckpointExists(t, s.Dir, cpID)

		// Validate checkpoint metadata completeness.
		meta := testutil.ReadCheckpointMetadata(t, s.Dir, cpID)
		assert.NotEmpty(t, meta.CLIVersion, "cli_version should be set")
		assert.NotEmpty(t, meta.Strategy, "strategy should be set")
		assert.NotEmpty(t, meta.Sessions, "should have at least 1 session")

		// Validate session metadata — agent field should be populated.
		sm := testutil.ReadSessionMetadata(t, s.Dir, cpID, 0)
		assert.NotEmpty(t, sm.Agent, "session agent field should be populated")
		assert.NotEmpty(t, sm.SessionID, "session_id should be set")
		testutil.WaitForNoShadowBranches(t, s.Dir, 10*time.Second)
	})
}
