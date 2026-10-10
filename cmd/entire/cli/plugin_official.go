package cli

import "slices"

// Telemetry is recorded only for plugin names listed here. Third-party
// plugin names can carry sensitive identifiers (project, vendor), so
// everything outside this allowlist is invoked silently — see gh's
// extension-telemetry posture for the reasoning. Match is case-sensitive
// and exact; the binary on disk is `entire-<name>`.
//
//nolint:gochecknoglobals // package-level allowlist; mutated by tests via snapshot/restore.
var officialPlugins = []string{
	// Add Entire-shipped plugin names here as they're released. Alphabetical;
	// each names the repository it ships from, since the binary on disk
	// (`entire-<name>`) is the only other clue to where it came from.
	"ci",          // entireio/entire-ci: customer-facing CI-integration management
	"graph",       // entireio/entire-graph: local code graph for symbol search and impact analysis
	"investigate", // entireio/entire-investigate: multi-agent investigation
	"run",         // entireio/entire-run: launch an Entire-enabled agent
	"upgrade",     // entireio/entire-upgrade: upgrade the installed Entire binary
}

func IsOfficialPlugin(name string) bool {
	return slices.Contains(officialPlugins, name)
}
