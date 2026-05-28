package cli

import (
	"strings"

	"github.com/spf13/cobra"

	entireprojectcli "github.com/entireio/cli/internal/entiredb/cmd/entire-project/cli"
)

// newProjectCmd wires the vendored entiredb `entire-project` plugin
// tree as a hidden top-level command, surfaced via `entire labs`.
// Project create / list talk to entire-core; no cluster URL required.
//
// Hidden during maturation; once stable drop the Hidden flag and
// remove the labs entry in one move.
func newProjectCmd() *cobra.Command {
	cmd := entireprojectcli.NewRootCmd()
	cmd.Use = "project"
	cmd.Hidden = true
	cmd.Annotations = map[string]string{
		cobra.CommandDisplayNameAnnotation: "entire project",
	}
	cmd.Long = strings.ReplaceAll(cmd.Long, "entire-project ", "entire project ")
	rebrandSubtreeExamples(cmd, "entire-project ", "entire project ")
	return cmd
}
