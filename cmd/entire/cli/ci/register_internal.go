//go:build internal

package ci

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newBuildkiteCmd and the four Buildkite verbs it mounts live in
// buildkite_internal.go, alongside the control-plane client seam and rendering
// helpers they share.

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
