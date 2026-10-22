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
	add("title", change.Title)
	add("body", change.Body)
	for _, field := range []struct{ name, value string }{
		{cmdStatus, change.Status}, {"type", change.Type}, {"priority", change.Priority},
	} {
		// Empty enum flags are rejected; omission preserves their empty wire values.
		if field.value != "" {
			add(field.name, field.value)
		}
	}
	// --assignee is a CSV flag. Quote each element before shell-quoting the value.
	assignees := make([]string, len(change.Assignees))
	for i, assignee := range change.Assignees {
		assignees[i] = `"` + strings.ReplaceAll(assignee, `"`, `""`) + `"`
	}
	add("assignee", strings.Join(assignees, ","))
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
