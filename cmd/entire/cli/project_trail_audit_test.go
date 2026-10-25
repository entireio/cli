package cli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectTrailStatusExcludesDraft(t *testing.T) {
	// If validation regresses, repository discovery must still stay isolated.
	t.Chdir(t.TempDir())
	for _, verb := range []string{"create", "update", "link", "list"} {
		t.Run(verb, func(t *testing.T) {
			args := []string{verb, "--status", "draft"}
			switch verb {
			case "create":
				args = append(args, "--title", "Intent", "--no-branch")
			case "update", "link":
				args = append(args, projectTrailTestID)
			}
			_, _, err := executeProjectTrailTest(t, args...)
			require.ErrorContains(t, err, "project trail status must be open or closed")
			cmd, _, err := newTrailCmdForMode(true).Find([]string{verb})
			require.NoError(t, err)
			require.NotContains(t, cmd.Flags().Lookup("status").Usage, "draft")
		})
	}
	for _, status := range []string{"open", "closed"} {
		require.True(t, validProjectTrailStatus(status))
	}
	require.False(t, validProjectTrailStatus("draft"))
}
