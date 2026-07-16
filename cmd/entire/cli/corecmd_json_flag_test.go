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
// mirror create/remove, grant remove) that ignored it, silently accepting a
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
		"repo create":                    true,
		"repo list":                      true,
		"repo get":                       true,
		"repo delete":                    false,
		"repo clone":                     false,
		"repo mirror create":             false,
		"repo mirror list":               true,
		"repo mirror get":                true,
		"repo mirror remove":             false,
		"repo mirror collaborators list": true,
		"repo visibility get":            true,
		"repo visibility set":            true,
		// grant
		"grant org add":        true,
		"grant org list":       true,
		"grant org remove":     false,
		"grant project add":    true,
		"grant project list":   true,
		"grant project remove": false,
		"grant repo add":       true,
		"grant repo list":      true,
		"grant repo remove":    false,
	}

	got := map[string]bool{}
	for _, root := range []*cobra.Command{newOrgCmd(), newProjectCmd(), newRepoCmd(), newGrantCmd()} {
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
