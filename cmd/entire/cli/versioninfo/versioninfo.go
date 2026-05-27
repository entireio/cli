package versioninfo

import (
	"runtime/debug"
	"strings"
)

// Version and Commit are set at build time via ldflags for release, nightly,
// and `mise run build` binaries. Plain `go build`/`go install` binaries leave
// these at their defaults and rely on the VCS metadata the Go toolchain embeds.
var (
	Version = "dev"
	Commit  = "unknown"
)

// dirtySuffix marks a build produced from a modified working tree. It matches
// the suffix the Go toolchain appends to a module pseudo-version.
const dirtySuffix = "+dirty"

// CheckpointVersion returns the build identity recorded in checkpoint metadata
// (the cli_version field on entire/checkpoints/v1). It mirrors Go's
// pseudo-version scheme so a checkpoint can be traced to the last known tag,
// the originating commit, and whether the working tree was dirty at build time.
//
// Release, nightly, and mise builds stamp Version via ldflags, so that value is
// authoritative; only a "+dirty" marker is added when the build tree was
// modified. Plain `go build` binaries leave Version at "dev"; for those we fall
// back to the module pseudo-version the Go toolchain embeds (e.g.
// "v0.6.3-...-15d80761c74b+dirty"), which already carries the tag, commit, and
// dirty marker.
func CheckpointVersion() string {
	return describe(Version, readBuildInfo())
}

// buildInfo is the subset of debug.BuildInfo that describe needs, extracted so
// the formatting logic can be exercised in tests without a real build.
type buildInfo struct {
	// pseudoVersion is debug.BuildInfo.Main.Version. It is empty or "(devel)"
	// when no module pseudo-version is available (e.g. test binaries).
	pseudoVersion string
	// modified reports whether vcs.modified was "true" at build time.
	modified bool
}

func readBuildInfo() buildInfo {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return buildInfo{}
	}
	info := buildInfo{pseudoVersion: bi.Main.Version}
	for _, s := range bi.Settings {
		if s.Key == "vcs.modified" {
			info.modified = s.Value == "true"
		}
	}
	return info
}

func describe(version string, bi buildInfo) string {
	// Without an ldflags stamp, defer to the Go-embedded pseudo-version: it
	// already encodes the last tag, the commit, and the +dirty marker.
	if version == "dev" && hasPseudoVersion(bi.pseudoVersion) {
		return bi.pseudoVersion
	}
	if bi.modified && !strings.HasSuffix(version, dirtySuffix) {
		return version + dirtySuffix
	}
	return version
}

func hasPseudoVersion(v string) bool {
	return v != "" && v != "(devel)"
}
