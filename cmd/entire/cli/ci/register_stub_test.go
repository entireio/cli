//go:build !internal

package ci_test

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/ci"
)

// TestRegister_NoopInPublicBuild asserts that without the `internal` build tag
// Register is a no-op: the `ci` group must not appear on the public binary.
func TestRegister_NoopInPublicBuild(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{Use: "entire"}
	ci.Register(root)

	for _, c := range root.Commands() {
		if c.Name() == "ci" {
			t.Fatal("public build: `ci` group must not be registered")
		}
	}
}
