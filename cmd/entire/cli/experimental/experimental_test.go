package experimental

import (
	"testing"

	"github.com/spf13/cobra"
)

// setVisible sets the package-global Visible for the duration of the test and
// restores it afterward. Mutating a global means these tests cannot run in
// parallel.
func setVisible(t *testing.T, v string) {
	t.Helper()
	prev := Visible
	Visible = v
	t.Cleanup(func() { Visible = prev })
}

func TestIsVisible(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"false", false},
		{"", true},         // only the literal "false" hides
		{"anything", true}, // any non-"false" stamp is treated as visible
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			setVisible(t, tt.value)
			if got := IsVisible(); got != tt.want {
				t.Fatalf("IsVisible() with Visible=%q = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestRegister_Visible(t *testing.T) {
	setVisible(t, "true")

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
	setVisible(t, "false")

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
	setVisible(t, "true")

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
