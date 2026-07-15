//go:build internal

package ci_test

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/ci"
)

// TestRegister_MountsCIGroup asserts that under the `internal` build tag the
// `ci` group is registered, stays Hidden, and carries the `buildkite`
// placeholder subgroup.
func TestRegister_MountsCIGroup(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{Use: "entire"}
	ci.Register(root)

	ciCmd := findCommand(root, "ci")
	if ciCmd == nil {
		t.Fatal("internal build: expected `ci` group to be registered on root")
	}
	if !ciCmd.Hidden {
		t.Error("`ci` group should be Hidden")
	}
	if findCommand(ciCmd, "buildkite") == nil {
		t.Error("internal build: expected `ci buildkite` subgroup to be present")
	}
}

func findCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
