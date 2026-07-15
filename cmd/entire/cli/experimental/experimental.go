// Package experimental gates the visibility of experimental CLI commands.
//
// Experimental commands stay fully runnable in every build; this package only
// controls whether they appear in `entire help`. Developer builds (go build,
// go run, mise) show them, grouped under an "Experimental commands:" help
// section. Release builds (GoReleaser) hide them.
package experimental

import "github.com/spf13/cobra"

// Visible controls whether experimental commands are shown in help. It is
// stamped by GoReleaser via ldflags
// (-X github.com/entireio/cli/cmd/entire/cli/experimental.Visible=false)
// to hide them in shipped binaries. It defaults to "true", so every
// non-release build (go build, go run, mise) shows them. The commands remain
// experimental and fully runnable regardless of this flag — it only toggles
// visibility.
var Visible = "true"

// IsVisible reports whether experimental commands are shown in help.
func IsVisible() bool { return Visible != "false" }

// GroupID is the cobra group experimental commands are filed under.
const GroupID = "experimental"

const groupTitle = "Experimental commands:"

// Register adds child under parent as an experimental command.
//
// When experimental commands are visible, child is filed under parent's
// "Experimental commands:" help group (registering the group on parent once).
// When hidden, child is marked Hidden and left ungrouped — so release help
// never carries an empty group header, and cobra never references a group ID
// that was not registered.
//
// Register overrides any Hidden value the child's constructor set, so callers
// do not need to touch the constructors (including ones in other packages).
func Register(parent, child *cobra.Command) {
	if IsVisible() {
		if !parent.ContainsGroup(GroupID) {
			parent.AddGroup(&cobra.Group{ID: GroupID, Title: groupTitle})
		}
		child.Hidden = false
		child.GroupID = GroupID
	} else {
		child.Hidden = true
		child.GroupID = ""
	}
	parent.AddCommand(child)
}
