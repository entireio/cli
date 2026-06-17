package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

func TestVersionFlag_OutputMatchesVersionCmd(t *testing.T) {
	t.Parallel()

	// Run "entire --version"
	root := NewRootCmd()
	var flagOut bytes.Buffer
	root.SetOut(&flagOut)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("entire --version failed: %v", err)
	}

	// Run "entire version"
	root2 := NewRootCmd()
	var cmdOut bytes.Buffer
	root2.SetOut(&cmdOut)
	root2.SetErr(&bytes.Buffer{})
	root2.SetArgs([]string{"version"})
	if err := root2.Execute(); err != nil {
		t.Fatalf("entire version failed: %v", err)
	}

	if flagOut.String() != cmdOut.String() {
		t.Errorf("output mismatch:\n--version: %q\nversion:   %q", flagOut.String(), cmdOut.String())
	}
}

func TestVersionFlag_ContainsExpectedInfo(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("entire --version failed: %v", err)
	}

	output := out.String()

	checks := []struct {
		name     string
		contains string
	}{
		{"version number", versioninfo.Version},
		{"go version", runtime.Version()},
		{"os", runtime.GOOS},
		{"arch", runtime.GOARCH},
	}
	for _, c := range checks {
		if !strings.Contains(output, c.contains) {
			t.Errorf("--version output missing %s (%q):\n%s", c.name, c.contains, output)
		}
	}
}

func TestPersistentPostRun_SkipsHiddenParent(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()

	// Find the leaf command: entire hooks git post-rewrite
	// This exercises the real command tree where "hooks" is Hidden but its descendants are not.
	leaf, _, err := root.Find([]string{"hooks", "git", "post-rewrite"})
	if err != nil {
		t.Fatalf("could not find hooks git post-rewrite command: %v", err)
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

func TestRoot_NounGroupShorthandsUseCobraAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		alias     string
		canonical string
	}{
		{alias: "sessions", canonical: "session"},
		{alias: "cp", canonical: "checkpoint"},
		{alias: "checkpoints", canonical: "checkpoint"},
	}

	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd()
			cmd, _, err := root.Find([]string{tt.alias})
			if err != nil {
				t.Fatalf("root.Find(%q): %v", tt.alias, err)
			}
			if cmd.Name() != tt.canonical {
				t.Fatalf("alias %q resolved to %q, want %q", tt.alias, cmd.Name(), tt.canonical)
			}
			if !containsString(cmd.Aliases, tt.alias) {
				t.Fatalf("%q should be registered in %q Aliases, got %v", tt.alias, tt.canonical, cmd.Aliases)
			}
			for _, direct := range root.Commands() {
				if direct.Name() == tt.alias {
					t.Fatalf("%q should be a Cobra alias, not a duplicate root command", tt.alias)
				}
			}
		})
	}
}

func TestCheckpointSearchIsVisibleButTopLevelSearchIsHidden(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()

	checkpointSearch, _, err := root.Find([]string{"checkpoint", "search"})
	if err != nil {
		t.Fatalf("find checkpoint search: %v", err)
	}
	if checkpointSearch.Hidden {
		t.Fatal("checkpoint search should be visible in checkpoint help")
	}

	topLevelSearch, _, err := root.Find([]string{"search"})
	if err != nil {
		t.Fatalf("find top-level search: %v", err)
	}
	if !topLevelSearch.Hidden {
		t.Fatal("top-level search should remain hidden as a compatibility alias")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
