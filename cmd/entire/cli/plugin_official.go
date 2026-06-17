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
	// Add Entire-shipped plugin names here as they're released.
	// e.g. "pgr"
}

func IsOfficialPlugin(name string) bool {
	return slices.Contains(officialPlugins, name)
}
