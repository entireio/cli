package versioninfo

import (
	"runtime/debug"
	"strings"
)

// Version and Commit identify the running CLI build.
//
// Only release binaries (GoReleaser) stamp these via ldflags
// (-X ...versioninfo.Version=...). Every other build -- `mise build`,
// `go install github.com/entireio/cli/cmd/entire@<version>`, and plain
// `go build`/`go install ./...` -- carries no ldflags, so Load() recovers
// them from Go's embedded build info instead and the CLI still self-reports
// a real version and commit.
var (
	Version = "dev"
	Commit  = "unknown"
)

// Load fills Version and Commit from the binary's build info when ldflags left
// them at their defaults. Call once from main() before either is read.
func Load() {
	info, ok := debug.ReadBuildInfo()
	Version, Commit = resolve(Version, Commit, info, ok)
}

// resolve fills Version/Commit from build info only when ldflags left them at
// their defaults; an explicit ldflags stamp always wins. A module install
// (`@<version>`) carries the version as info.Main.Version; a local build
// reports "(devel)" there but records the commit under vcs.revision. (Go
// already marks a dirty tree with a "+dirty" suffix on the version, so we
// don't track vcs.modified ourselves.)
func resolve(version, commit string, info *debug.BuildInfo, ok bool) (string, string) {
	if version != "dev" || !ok || info == nil {
		return version, commit
	}

	if v := info.Main.Version; v != "" && v != "(devel)" {
		version = strings.TrimPrefix(v, "v") // match GoReleaser's {{.Version}}
	}
	// Only fill an unset commit; an explicit ldflags stamp always wins.
	if commit == "unknown" {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				commit = setting.Value
			}
		}
	}

	return version, commit
}
