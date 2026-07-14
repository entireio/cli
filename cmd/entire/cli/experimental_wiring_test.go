package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/experimental"
	"github.com/spf13/cobra"
)

// experimentalRootCommands are the top-level commands gated behind the
// experimental visibility flag. Names match cobra's Command.Name() (the first
// token of Use).
var experimentalRootCommands = []string{
	"tokens", "import", "review", "investigate",
	"blame", "why", "search", "experts", "runner",
}

// withVisible sets the experimental visibility flag for the test and restores
// it afterward. Because it mutates a package global, callers must not run in
// parallel.
func withVisible(t *testing.T, v string) {
	t.Helper()
	prev := experimental.Visible
	experimental.Visible = v
	t.Cleanup(func() { experimental.Visible = prev })
}

func findCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// checkpointPolicy returns the `checkpoint policy` command.
func checkpointPolicy(t *testing.T, root *cobra.Command) *cobra.Command {
	t.Helper()
	cp := findCommand(root, "checkpoint")
	if cp == nil {
		t.Fatal("checkpoint command not found on root")
	}
	return findCommand(cp, "policy")
}

// TestExperimental_VisibleInDevBuild verifies that, in a developer build
// (Visible defaults to "true"), the experimental commands are shown and filed
// under the experimental group. Cannot use t.Parallel — mutates a global.
func TestExperimental_VisibleInDevBuild(t *testing.T) {
	withVisible(t, "true")

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

	policy := checkpointPolicy(t, root)
	if policy == nil {
		t.Fatal("checkpoint policy not found")
	}
	if policy.Hidden {
		t.Error("checkpoint policy should be visible in a dev build")
	}
	if policy.GroupID != experimental.GroupID {
		t.Errorf("checkpoint policy GroupID = %q, want %q", policy.GroupID, experimental.GroupID)
	}
}

// TestExperimental_HiddenInReleaseBuild verifies that, when GoReleaser stamps
// Visible=false, the experimental commands are hidden, carry no group, and the
// empty experimental group is never registered (so release help is unchanged).
// Cannot use t.Parallel — mutates a global.
func TestExperimental_HiddenInReleaseBuild(t *testing.T) {
	withVisible(t, "false")

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

	policy := checkpointPolicy(t, root)
	if policy == nil {
		t.Fatal("checkpoint policy not found")
	}
	if !policy.Hidden {
		t.Error("checkpoint policy should be hidden in a release build")
	}
}
