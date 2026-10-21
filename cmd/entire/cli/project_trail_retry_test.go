package cli

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// The shell executes the printed line unchanged, with an inert entire function
// recording argv; the caller passes those arguments to the in-process test CLI.
func executeTrailRetryShell(t *testing.T, command string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		pwsh, err := exec.LookPath("pwsh")
		if err != nil {
			t.Skip("PowerShell is required to test the Windows copy-paste command")
		}
		out, err := execx.NonInteractive(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-Command",
			"function entire { ConvertTo-Json -InputObject @($args) -Compress }; "+command).CombinedOutput()
		require.NoError(t, err, string(out))
		var args []string
		require.NoError(t, json.Unmarshal(out, &args))
		return args
	}
	out, err := execx.NonInteractive(t.Context(), "sh", "-c",
		"entire() { printf '%s\\000' \"$@\"; }\n"+command).CombinedOutput()
	require.NoError(t, err, string(out))
	require.True(t, strings.HasSuffix(string(out), "\x00"))
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

func TestProjectTrailLinkRetryPreservesExecutionOptions(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.Flags().String(contextFlagName, "test 'context'", "")
	cmd.Flags().Bool("insecure-http-auth", true, "")
	cmd.Flags().Bool("json", true, "")
	target := &projectTrailTarget{TrailID: projectTrailTestID, Host: "et", Project: "acme", RepoRef: "et/acme/widget"}
	key := "retry 'key' $value"
	hint := projectTrailLinkRetry(cmd, target, api.ChangeCreateRequest{Title: "Intent", BranchName: "feature/work", BranchAction: "link"}, key)
	_, command, found := strings.Cut(hint, ":\n")
	require.True(t, found)
	args := executeTrailRetryShell(t, command)
	for _, arg := range []string{"--context=test 'context'", "--insecure-http-auth", "--json", "--idempotency-key=" + key, "--repo=et/acme/widget", "--project=et/acme", "--body=", "--base=", "--assignee="} {
		require.Contains(t, args, arg)
	}
	for _, name := range []string{"status", "type", "priority"} {
		require.NotContains(t, args, "--"+name+"=", "empty enum flags fail validation")
	}
}
