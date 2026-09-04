//go:build unix

package versioncheck

import "strings"

func hasNormalizedRootPrefix(path, root string) bool {
	return strings.HasPrefix(path, root)
}

func brewUpgradeCommand(_ string, currentVersion string) string {
	if releaseChannel(currentVersion) == installChannelNightly {
		return "brew upgrade --yes entire@nightly"
	}
	return "brew upgrade --yes entire"
}

func noInstallRoots() []string { return nil }

var brewProbe = installProbe{
	roots: noInstallRoots,
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
	if releaseChannel(currentVersion) == installChannelNightly {
		return "curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly"
	}
	return "curl -fsSL https://entire.io/install.sh | bash"
}
