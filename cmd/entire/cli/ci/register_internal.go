//go:build internal

package ci

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Register mounts the hidden `ci` command group onto root. This variant is
// compiled only under the `internal` build tag; the `!internal` build gets the
// no-op twin in register_stub.go, so the public `entire` binary omits the group
// entirely. Both files export the same Register symbol, so root.go compiles
// unchanged under either tag.
func Register(root *cobra.Command) {
	root.AddCommand(newCICmd())
}

// newCICmd is the bare `entire ci` parent: it carries the shared control-plane
// persistent flags and mounts the CI-integration subgroups (currently just
// `buildkite`), mirroring the control-plane groups in the cli package such as
// newRepoCmd. It is marked Hidden as belt-and-suspenders — the build tag
// already keeps the whole group out of the public binary, and Hidden keeps it
// out of help within internal builds until the verbs land in later PRs.
func newCICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "ci",
		Short:  "Manage Entire CI integrations (internal)",
		Hidden: true,
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newBuildkiteCmd())
	return cmd
}

// newBuildkiteCmd is the placeholder `entire ci buildkite` subgroup. The
// Buildkite verbs (list/create/…) stack onto it in later PRs; for now bare
// invocation prints a "coming soon" notice so the wiring is exercised
// end-to-end.
func newBuildkiteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "buildkite",
		Short: "Manage Buildkite CI webhook integrations (coming soon)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// cobra's Print* writes to stderr in production, so use Fprintln
			// against OutOrStdout explicitly (enforced by the forbidigo linter).
			fmt.Fprintln(cmd.OutOrStdout(), "entire ci buildkite: coming soon")
			return nil
		},
	}
}

// addControlPlaneFlags registers the persistent flags shared by the
// control-plane command groups, mirroring cli.addControlPlaneFlags. It is
// replicated here rather than imported because the cli package imports this
// package (root.go calls ci.Register), so importing cli back would form an
// import cycle.
func addControlPlaneFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().Bool("insecure-http-auth", false, "Allow authentication over plain HTTP (insecure, for local development only)")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}
}
