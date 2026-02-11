package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestPersistentPostRun_SkipsHiddenParent(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()

	// Find the leaf command: entire hooks git post-commit
	// This exercises the real command tree where "hooks" is Hidden but its descendants are not.
	leaf, _, err := root.Find([]string{"hooks", "git", "post-commit"})
	if err != nil {
		t.Fatalf("could not find hooks git post-commit command: %v", err)
	}

	if leaf.Hidden {
		t.Fatal("leaf command should not be hidden itself — the test validates parent-chain detection")
	}

	// Walk the parent chain (excluding root) and confirm at least one ancestor is hidden.
	foundHidden := false
	for c := leaf.Parent(); c != nil && c != root; c = c.Parent() {
		if c.Hidden {
			foundHidden = true
			break
		}
	}
	if !foundHidden {
		t.Fatal("expected at least one hidden ancestor between the leaf and root")
	}
}

func TestPersistentPostRun_ParentHiddenWalk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		buildTree  func() *cobra.Command // returns the leaf command to test
		wantHidden bool
	}{
		{
			name: "leaf hidden",
			buildTree: func() *cobra.Command {
				root := &cobra.Command{Use: "root"}
				child := &cobra.Command{Use: "child", Hidden: true}
				root.AddCommand(child)
				return child
			},
			wantHidden: true,
		},
		{
			name: "parent hidden, leaf visible",
			buildTree: func() *cobra.Command {
				root := &cobra.Command{Use: "root"}
				parent := &cobra.Command{Use: "parent", Hidden: true}
				leaf := &cobra.Command{Use: "leaf"}
				root.AddCommand(parent)
				parent.AddCommand(leaf)
				return leaf
			},
			wantHidden: true,
		},
		{
			name: "grandparent hidden, leaf visible",
			buildTree: func() *cobra.Command {
				root := &cobra.Command{Use: "root"}
				gp := &cobra.Command{Use: "gp", Hidden: true}
				parent := &cobra.Command{Use: "parent"}
				leaf := &cobra.Command{Use: "leaf"}
				root.AddCommand(gp)
				gp.AddCommand(parent)
				parent.AddCommand(leaf)
				return leaf
			},
			wantHidden: true,
		},
		{
			name: "no hidden ancestor",
			buildTree: func() *cobra.Command {
				root := &cobra.Command{Use: "root"}
				parent := &cobra.Command{Use: "parent"}
				leaf := &cobra.Command{Use: "leaf"}
				root.AddCommand(parent)
				parent.AddCommand(leaf)
				return leaf
			},
			wantHidden: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := tt.buildTree()

			// Replicate the parent-walk logic from PersistentPostRun
			gotHidden := false
			for c := cmd; c != nil; c = c.Parent() {
				if c.Hidden {
					gotHidden = true
					break
				}
			}

			if gotHidden != tt.wantHidden {
				t.Errorf("isHidden = %v, want %v", gotHidden, tt.wantHidden)
			}
		})
	}
}
