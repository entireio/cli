//go:build unix

package versioncheck

// foldPathCase is the identity on unix: paths compare case-sensitively.
func foldPathCase(p string) string { return p }

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

var installProbes = []installProbe{brewProbe, miseProbe}

func fallbackInstallCommand(currentVersion string) string {
	if isNightly(currentVersion) {
		return "curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly"
	}
	return "curl -fsSL https://entire.io/install.sh | bash"
}
