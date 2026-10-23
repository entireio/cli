package experimental

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

// Version strings standing in for the build kinds that matter here.
const (
	devVersion    = "dev"
	stableVersion = "0.12.0"
)

// setVersion stamps versioninfo.Version for the duration of the test and
// restores it afterward. Mutating a global means these tests cannot run in
// parallel.
func setVersion(t *testing.T, v string) {
	t.Helper()
	prev := versioninfo.Version
	versioninfo.Version = v
	t.Cleanup(func() { versioninfo.Version = prev })
}

func TestIsVisible(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{devVersion, true},           // unstamped local build
		{"", true},                   // defensive: never hide on an empty stamp
		{stableVersion, false},       // GoReleaser / nixpkgs stable
		{"v" + stableVersion, false}, // tag-shaped stamp still counts as stable
		{"0.12.1-nightly.202610211647.29f996951b", true}, // nightly channel
		{"0.0.0+29f996951b", true},                       // nonprod commit build
		{"0.12.0+dirty", true},                           // build metadata is never a release
		{"0.12", true},                                   // not a semver, so not a release
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			setVersion(t, tt.version)
			if got := IsVisible(); got != tt.want {
				t.Fatalf("IsVisible() with Version=%q = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestRegister_Visible(t *testing.T) {
	setVersion(t, devVersion)

	parent := &cobra.Command{Use: "parent"}
	child := &cobra.Command{Use: "child", Hidden: true} // constructor-set Hidden must be overridden
	Register(parent, child)

	if child.Hidden {
		t.Error("child should be visible when experimental commands are visible")
	}
	if child.GroupID != GroupID {
		t.Errorf("child.GroupID = %q, want %q", child.GroupID, GroupID)
	}
	if !parent.ContainsGroup(GroupID) {
		t.Error("parent should have the experimental group registered")
	}
	if len(parent.Commands()) != 1 || parent.Commands()[0] != child {
		t.Error("child should be added to parent")
	}
}

func TestRegister_Hidden(t *testing.T) {
	setVersion(t, stableVersion)

	parent := &cobra.Command{Use: "parent"}
	child := &cobra.Command{Use: "child"}
	Register(parent, child)

	if !child.Hidden {
		t.Error("child should be hidden when experimental commands are hidden")
	}
	if child.GroupID != "" {
		t.Errorf("child.GroupID = %q, want empty (no group referenced in release)", child.GroupID)
	}
	if parent.ContainsGroup(GroupID) {
		t.Error("parent should not register the experimental group in release builds")
	}
	if len(parent.Commands()) != 1 || parent.Commands()[0] != child {
		t.Error("child should still be added to parent")
	}
}

// TestRegister_MultipleShareOneGroup verifies the group is registered once even
// when several experimental commands are registered under the same parent.
func TestRegister_MultipleShareOneGroup(t *testing.T) {
	setVersion(t, devVersion)

	parent := &cobra.Command{Use: "parent"}
	Register(parent, &cobra.Command{Use: "a"})
	Register(parent, &cobra.Command{Use: "b"})

	groups := parent.Groups()
	count := 0
	for _, g := range groups {
		if g.ID == GroupID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("experimental group registered %d times, want 1", count)
	}
}
