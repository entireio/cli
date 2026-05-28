package cli

import (
	"strings"

	"github.com/spf13/cobra"

	entirerepocli "github.com/entireio/cli/internal/entiredb/cmd/entire-repo/cli"
)

// newRepoCmd wires the vendored entiredb `entire-repo` plugin tree as a
// hidden top-level command, surfaced via `entire labs`. The plugin owns
// every user-facing repo verb (lifecycle, content, mirror placement)
// against EntireDB clusters; see labs.go for the labs-visible summary.
//
// The plugin's own root is named "entire-repo" (the kubectl-style binary
// name in entiredb's own deploy layout) — rename it to "repo" and pin
// the display name so cobra's CommandPath, UseLine, and `entire repo
// --help` output read as "entire repo …" rather than "entire-repo …".
//
// Hidden during maturation: the command is functional from `entire repo`
// but does not appear in `entire --help`. Once stable, drop the Hidden
// flag and remove the labs entry in one move.
//
// Auth flows through ~/.config/entire/contexts.json, populated by both
// `entire login` (after the cmd/entire/cli/login_entiredb_context.go
// bridge) and the upstream `entire-core auth login`. Lifecycle verbs
// talk to entire-core (no cluster URL required); data-plane verbs take
// the cluster as part of the positional (e.g. `<cluster>/et/<org>/<repo>`).
func newRepoCmd() *cobra.Command {
	cmd := entirerepocli.NewRootCmd()
	cmd.Use = "repo"
	cmd.Hidden = true
	cmd.Annotations = map[string]string{
		cobra.CommandDisplayNameAnnotation: "entire repo",
	}
	cmd.Long = strings.ReplaceAll(cmd.Long, "entire-repo ", "entire repo ")
	rebrandSubtreeExamples(cmd, "entire-repo ", "entire repo ")
	return cmd
}

// rebrandSubtreeExamples rewrites every Example block in cmd's subtree,
// replacing the upstream kubectl-style prefix (e.g. "entire-repo ")
// with the dispatched form ("entire repo "). The example strings on
// vendored entiredb plugin subcommands are authored against their
// standalone binary names; once mounted under the cli's root they need
// the form users actually type.
func rebrandSubtreeExamples(cmd *cobra.Command, from, to string) {
	if cmd.Example != "" {
		cmd.Example = strings.ReplaceAll(cmd.Example, from, to)
	}
	for _, c := range cmd.Commands() {
		rebrandSubtreeExamples(c, from, to)
	}
}
