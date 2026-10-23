package cli

import (
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func projectTrailLinkRetry(cmd *cobra.Command, target *projectTrailTarget, change api.ChangeCreateRequest, key string) string {
	shell := "a POSIX shell"
	if runtime.GOOS == windowsGOOS {
		shell = "PowerShell"
	}
	args := []string{"entire " + cmdTrail + " link", target.TrailID}
	add := func(name, value string) {
		quoted := shellQuote(value)
		if runtime.GOOS == windowsGOOS {
			quoted = powerShellQuote(value)
		}
		args = append(args, "--"+name+"="+quoted)
	}
	add("repo", target.RepoRef)
	add("project", target.Host+"/"+target.Project)
	add("branch", change.BranchName)
	add("branch-action", change.BranchAction)
	add("base", change.Base)
	// An empty enum flag is rejected; omission preserves the empty wire value.
	if change.Status != "" {
		add(cmdStatus, change.Status)
	}
	add("idempotency-key", key)
	if flag := cmd.Flags().Lookup(contextFlagName); flag != nil && flag.Value.String() != "" {
		add(contextFlagName, flag.Value.String())
	}
	if trailInsecureHTTP(cmd) {
		args = append(args, "--insecure-http-auth")
	}
	if jsonRequested(cmd) {
		args = append(args, "--json")
	}
	return "Retry in " + shell + ":\n" + strings.Join(args, " ")
}
