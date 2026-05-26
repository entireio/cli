//go:build e2e

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDoctorNoIssues verifies the manual-plan "no issues detected" scenario.
// After a normal checkpointed commit and push, doctor should report clean
// metadata/session health.
func TestDoctorNoIssues(t *testing.T) {
	testutil.ForEachNamedAgent(t, 3*time.Minute, []string{"vogon"}, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_ = testutil.SetupBareRemote(t, s)

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")
		s.Git(t, "push")

		_, err := s.RunPrompt(t, ctx,
			"create a file at docs/doctor.md with a short paragraph about checkpoint health. Do not ask for confirmation or approval, just make the change.")
		require.NoError(t, err, "agent failed")

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add doctor coverage doc")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		s.Git(t, "push")
		testutil.PushCheckpointRefs(t, s.Dir)

		out := entire.Doctor(t, s.Dir)

		assert.Contains(t, out, "Metadata branches: OK", "doctor should report healthy metadata state")
		assert.Contains(t, out, "No stuck sessions found.", "doctor should report no stuck sessions")
		assert.NotContains(t, out, "v2 refs", "doctor should not run v2 doctor checks")
		assert.NotContains(t, out, "v2 checkpoint counts", "doctor should not run v2 count checks")
		assert.NotContains(t, out, "v2 generations", "doctor should not run v2 generation checks")
		assert.NotContains(t, out, "v2 /main ref", "doctor should not run v2 connectivity checks")
	})
}
