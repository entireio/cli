package cli

import (
	"sort"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestControlPlaneJSONFlag_OnlyOnHonoringCommands pins the structural fix that
// moved --json off the shared control-plane persistent flag and onto a local
// flag registered only where the command actually renders JSON.
//
// The old design registered --json persistently on each group root, so it was
// inherited by every subcommand — including side-effect verbs (delete, clone,
// mirror add/remove, grant remove) that ignored it, silently accepting a
// no-op flag. Now the flag exists exactly on the commands that honor it, so the
// non-honoring commands reject --json with "unknown flag" and their help never
// advertises it.
func TestControlPlaneJSONFlag_OnlyOnHonoringCommands(t *testing.T) {
	t.Parallel()

	// path (relative to the group root) -> honors --json.
	want := map[string]bool{
		// org
		"org create": true,
		"org list":   true,
		"org get":    true,
		"org delete": false,
		// project
		"project create": true,
		"project list":   true,
		"project get":    true,
		"project delete": false,
		// repo
		"repo create":        true,
		"repo list":          true,
		"repo view":          true,
		"repo edit":          true,
		"repo delete":        false,
		"repo clone":         false,
		"repo remote url":    false,
		"repo mirror add":    false,
		"repo mirror list":   true,
		"repo mirror get":    true,
		"repo mirror remove": false,
		// `remote use` writes local git config and reports what it changed;
		// there is no object to render, so it stays off the --json surface like
		// the other side-effect verbs.
		"repo remote use":     false,
		"repo access list":    true,
		"repo visibility get": true,
		// add/remove print the resulting rule list, so they render JSON too.
		"repo protection list":   true,
		"repo protection add":    true,
		"repo protection remove": true,
		// grant subtrees: add/list render a payload, remove only reports
		"org grant add":        true,
		"org grant list":       true,
		"org grant remove":     false,
		"project grant add":    true,
		"project grant list":   true,
		"project grant remove": false,
		"repo grant add":       true,
		"repo grant list":      true,
		"repo grant remove":    false,
	}

	got := map[string]bool{}
	for _, root := range []*cobra.Command{newOrgCmd(), newProjectCmd(), newRepoCmd()} {
		collectJSONFlag(t, root, root.Name(), got)
	}

	// Every command we expect an answer for must exist in the tree, and vice
	// versa — a drift in either direction (renamed/removed command, or a new
	// leaf we forgot to classify) should fail loudly.
	require.Equal(t, sortedKeys(want), sortedKeys(got), "command tree drifted from the expected --json map")
	for path, expected := range want {
		require.Equal(t, expected, got[path], "command %q: --json presence mismatch", path)
	}
}

// collectJSONFlag walks the command tree rooted at cmd, recording for each leaf
// command whether --json is visible on it (local flags merged with inherited).
func collectJSONFlag(t *testing.T, cmd *cobra.Command, path string, out map[string]bool) {
	t.Helper()
	children := cmd.Commands()
	if len(children) == 0 {
		// Merge parent persistent flags so an accidentally-inherited --json is
		// still caught here, not just a locally-registered one.
		out[path] = cmd.Flags().Lookup("json") != nil || cmd.InheritedFlags().Lookup("json") != nil
		return
	}
	for _, child := range children {
		collectJSONFlag(t, child, path+" "+child.Name(), out)
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
