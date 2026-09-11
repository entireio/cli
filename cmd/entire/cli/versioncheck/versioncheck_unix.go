//go:build unix

package versioncheck

// foldPathCase is the identity on unix: paths compare case-sensitively.
func foldPathCase(p string) string { return p }

// updateCommandShell is empty on unix: brew, mise, and the curl|bash one-liner
// all run in any POSIX shell, so no message has to name one.
const updateCommandShell = ""

func brewUpgradeCommand(_ string, currentVersion string) string {
	if isNightly(currentVersion) {
		return "brew upgrade --yes entire@nightly"
	}
	return "brew upgrade --yes entire"
}

var brewProbe = installProbe{
	markers: []string{
		"/Caskroom/",
		"/opt/homebrew/",
		"/linuxbrew/",
		"/Cellar/", // defensive: entire ships as a cask, not a formula
	},
	command: brewUpgradeCommand,
}

// installProbes order is load-bearing: UpdateCommandForCurrentBinary returns
// the FIRST match, and roots match by prefix, so a mise root wide enough to
// cover a brew path would claim it if it came first. Pinned by
// TestUnixBrewBeatsAMiseRootCoveringTheSamePath.
var installProbes = []installProbe{brewProbe, miseProbe}

// Every command this file returns is a compile-time literal, and that is load
// bearing rather than incidental: realRunInstaller hands the result to `sh -c`.
// The version and the executable path select among these commands; they must
// never appear inside one. TestUpdateCommandIsAlwaysALiteral enforces it — a
// command that needs a runtime value has to be built and run as argv instead.
// The Windows fallback is deliberately not held to this: it interpolates the
// install directory, and Windows never runs the installer (realRunInstaller is
// unimplemented there, so the command is only ever printed).
func fallbackInstallCommand(_, currentVersion string) string {
	if isNightly(currentVersion) {
		return "curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly"
	}
	return "curl -fsSL https://entire.io/install.sh | bash"
}
