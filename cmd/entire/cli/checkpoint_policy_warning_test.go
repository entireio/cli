package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestWarnCheckpointPolicyIfNeeded(t *testing.T) {
	_, _ = setupCheckpointPolicyRepo(t)
	repo, err := git.PlainOpen(".")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})
	_, err = checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "refs-v2",
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	WarnCheckpointPolicyIfNeeded(context.Background(), &buf, "1.0.0")

	require.Contains(t, buf.String(), "requires checkpoint support newer than this Entire CLI")
}

func TestShouldCheckCheckpointPolicyWarning(t *testing.T) {
	root := &cobra.Command{Use: "entire"}
	visible := &cobra.Command{Use: "status"}
	root.AddCommand(visible)

	hooks := &cobra.Command{Use: "hooks", Hidden: true}
	gitHook := &cobra.Command{Use: "git"}
	hooks.AddCommand(gitHook)
	root.AddCommand(hooks)

	hiddenAlias := &cobra.Command{Use: "explain", Hidden: true}
	root.AddCommand(hiddenAlias)

	sendAnalytics := &cobra.Command{Use: "__send_analytics", Hidden: true}
	root.AddCommand(sendAnalytics)

	require.True(t, ShouldCheckCheckpointPolicyWarning(visible))
	require.True(t, ShouldCheckCheckpointPolicyWarning(hiddenAlias))
	require.False(t, ShouldCheckCheckpointPolicyWarning(gitHook))
	require.False(t, ShouldCheckCheckpointPolicyWarning(sendAnalytics))
}
