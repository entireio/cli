package cli

import (
	"strings"

	"github.com/spf13/cobra"

	entireorgcli "github.com/entireio/cli/internal/entiredb/cmd/entire-org/cli"
)

// newOrgCmd wires the vendored entiredb `entire-org` plugin tree as a
// hidden top-level command, surfaced via `entire labs`. Org create /
// list talk to entire-core; no cluster URL required.
//
// The plugin's own root is named "entire-org" (the kubectl-style binary
// name in entiredb's deploy layout) — rename to "org" and pin the
// display name so cobra reports "entire org …" in help / docs / error
// paths. Hidden during maturation; once stable drop the Hidden flag and
// remove the labs entry in one move.
func newOrgCmd() *cobra.Command {
	cmd := entireorgcli.NewRootCmd()
	cmd.Use = "org"
	cmd.Hidden = true
	cmd.Annotations = map[string]string{
		cobra.CommandDisplayNameAnnotation: "entire org",
	}
	cmd.Long = strings.ReplaceAll(cmd.Long, "entire-org ", "entire org ")
	rebrandSubtreeExamples(cmd, "entire-org ", "entire org ")
	return cmd
}
