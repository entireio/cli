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
// Auth and cluster routing are deliberately not bridged here. The plugin
// follows the entiredb model: `entire-core auth login` writes
// ~/.config/entire/contexts.json; lifecycle verbs talk to entire-core
// (no cluster URL required), and data-plane verbs take the cluster as
// part of the positional (e.g. `<cluster>/et/<org>/<repo>`). The cli's
// own `entire login` populates a different keyring service and is not
// (yet) wired into this surface.
func newRepoCmd() *cobra.Command {
	cmd := entirerepocli.NewRootCmd()
	cmd.Use = "repo"
	cmd.Hidden = true
	cmd.Annotations = map[string]string{
		cobra.CommandDisplayNameAnnotation: "entire repo",
	}
	cmd.Long = strings.ReplaceAll(cmd.Long, "entire-repo ", "entire repo ")
	rebrandRepoExamples(cmd)
	return cmd
}

// rebrandRepoExamples rewrites every Example block in the entire-repo
// subtree so help and docs teach `entire repo …` rather than the
// upstream binary's `entire-repo …` form. The example strings are
// authored against the kubectl-style standalone binary; once mounted
// under the cli's root they need the dispatched command path the user
// actually types.
func rebrandRepoExamples(cmd *cobra.Command) {
	if cmd.Example != "" {
		cmd.Example = strings.ReplaceAll(cmd.Example, "entire-repo ", "entire repo ")
	}
	for _, c := range cmd.Commands() {
		rebrandRepoExamples(c)
	}
}
