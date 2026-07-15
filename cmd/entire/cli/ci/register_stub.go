//go:build !internal

package ci

import "github.com/spf13/cobra"

// Register is the no-op public-build twin of the internal Register
// (register_internal.go). Because both files export the same symbol, root.go's
// ci.Register(cmd) call compiles under both build tags; only the internal build
// actually mounts the `ci` group, so it is absent from released binaries.
func Register(_ *cobra.Command) {}
