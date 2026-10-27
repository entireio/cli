package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/experimental"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

// experimentalRootCommands are the top-level commands gated behind the
// experimental visibility flag. Names match cobra's Command.Name() (the first
// token of Use).
var experimentalRootCommands = []string{
	"tokens", "import", "review",
	"blame", "why", "experts", "runner",
}

// Version stamps standing in for a developer build and a stable release.
// Experimental visibility is derived from the resolved version.
const (
	devBuildVersion = "dev"
	releaseVersion  = "0.12.0"
)

// withBuildVersion stamps versioninfo.Version for the test and restores it
// afterward. Because it mutates a package global, callers must not run in
// parallel.
func withBuildVersion(t *testing.T, v string) {
	t.Helper()
	prev := versioninfo.Version
	versioninfo.Version = v
	t.Cleanup(func() { versioninfo.Version = prev })
}

func findCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestExperimental_VisibleInDevBuild verifies that, in a developer build
// (version "dev"), the experimental commands are shown and filed under the
// experimental group. Cannot use t.Parallel — mutates a global.
func TestExperimental_VisibleInDevBuild(t *testing.T) {
	withBuildVersion(t, devBuildVersion)

	root := NewRootCmd()

	if !root.ContainsGroup(experimental.GroupID) {
		t.Fatal("root should register the experimental group in a dev build")
	}
	for _, name := range experimentalRootCommands {
		cmd := findCommand(root, name)
		if cmd == nil {
			t.Errorf("%q not found on root", name)
			continue
		}
		if cmd.Hidden {
			t.Errorf("%q should be visible in a dev build", name)
		}
		if cmd.GroupID != experimental.GroupID {
			t.Errorf("%q GroupID = %q, want %q", name, cmd.GroupID, experimental.GroupID)
		}
	}
}

// TestExperimental_HiddenInReleaseBuild verifies that, when the build carries
// a stable release version, the experimental commands are hidden, carry no
// group, and the empty experimental group is never registered (so release help
// is unchanged). Cannot use t.Parallel — mutates a global.
func TestExperimental_HiddenInReleaseBuild(t *testing.T) {
	withBuildVersion(t, releaseVersion)

	root := NewRootCmd()

	if root.ContainsGroup(experimental.GroupID) {
		t.Error("root should not register the experimental group in a release build")
	}
	for _, name := range experimentalRootCommands {
		cmd := findCommand(root, name)
		if cmd == nil {
			t.Errorf("%q not found on root", name)
			continue
		}
		if !cmd.Hidden {
			t.Errorf("%q should be hidden in a release build", name)
		}
		if cmd.GroupID != "" {
			t.Errorf("%q GroupID = %q, want empty in a release build", name, cmd.GroupID)
		}
	}
}
