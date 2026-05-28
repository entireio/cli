package cli

import (
	"strings"

	"github.com/spf13/cobra"

	entiregrantcli "github.com/entireio/cli/internal/entiredb/cmd/entire-grant/cli"
)

// newGrantCmd wires the vendored entiredb `entire-grant` plugin tree
// as a hidden top-level command, surfaced via `entire labs`. Manages
// membership and access grants across repo / project / org layers;
// every verb hits entire-core, no cluster URL required.
//
// Hidden during maturation; once stable drop the Hidden flag and
// remove the labs entry in one move.
func newGrantCmd() *cobra.Command {
	cmd := entiregrantcli.NewRootCmd()
	cmd.Use = "grant"
	cmd.Hidden = true
	cmd.Annotations = map[string]string{
		cobra.CommandDisplayNameAnnotation: "entire grant",
	}
	cmd.Long = strings.ReplaceAll(cmd.Long, "entire-grant ", "entire grant ")
	rebrandSubtreeExamples(cmd, "entire-grant ", "entire grant ")
	return cmd
}
