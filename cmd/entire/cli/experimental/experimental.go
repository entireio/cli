// Package experimental gates the visibility of experimental CLI commands.
//
// Experimental commands stay fully runnable in every build; this package only
// controls whether they appear in `entire help`. Developer builds (go build,
// go run, mise) and nightlies show them, grouped under an "Experimental
// commands:" help section. Stable release builds hide them.
package experimental

import (
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

// IsVisible reports whether experimental commands are shown in help.
//
// Visibility follows the build's resolved version rather than a separate
// stamp, so every packager that stamps versioninfo.Version (GoReleaser,
// nixpkgs, `go install @<tag>`) gets release behavior for free. Only a stable
// release hides them; "dev", nightlies, and commit-stamped builds show them.
// Call after versioninfo.Load() so the version is resolved.
func IsVisible() bool { return !isStableRelease(versioninfo.Version) }

// isStableRelease reports whether version is a canonical X.Y.Z release:
// no prerelease segment (-nightly.*) and no build metadata (+<commit>).
// Anything else, including a short "0.12" form, counts as a dev build.
func isStableRelease(version string) bool {
	tag := "v" + strings.TrimPrefix(version, "v")
	return semver.IsValid(tag) && semver.Canonical(tag) == tag && semver.Prerelease(tag) == ""
}

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
