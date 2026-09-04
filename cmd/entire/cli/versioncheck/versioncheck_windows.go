//go:build windows

package versioncheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// foldPathCase lowercases: Windows paths compare case-insensitively, and the
// markers below are already lowercase.
func foldPathCase(p string) string { return strings.ToLower(p) }

// Windows install.ps1 one-liners from the README. Printed, never auto-run.
const (
	windowsInstallCmd        = "irm https://raw.githubusercontent.com/entireio/cli/main/scripts/install.ps1 -UseBasicParsing | iex"
	windowsInstallNightlyCmd = "& ([scriptblock]::Create((irm https://raw.githubusercontent.com/entireio/cli/main/scripts/install.ps1 -UseBasicParsing))) -Channel nightly"
)

// scoopConfigPath is Scoop's config.json, which records relocated install roots.
func scoopConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "scoop", "config.json")
	}
	profile := os.Getenv("USERPROFILE")
	if profile == "" {
		return ""
	}
	return filepath.Join(profile, ".config", "scoop", "config.json")
}

type scoopConfigFile struct {
	RootPath   string `json:"root_path"`
	GlobalPath string `json:"global_path"`
}

func readScoopConfig() scoopConfigFile {
	path := scoopConfigPath()
	if path == "" {
		return scoopConfigFile{}
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed per-user config location, not user input
	if err != nil {
		return scoopConfigFile{}
	}
	var cfg scoopConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return scoopConfigFile{}
	}
	return cfg
}

func scoopRoots() []string {
	cfg := readScoopConfig()
	var roots []string
	for _, r := range []string{os.Getenv("SCOOP"), os.Getenv("SCOOP_GLOBAL"), cfg.RootPath, cfg.GlobalPath} {
		if r != "" {
			roots = append(roots, filepath.Join(r, "apps"))
		}
	}
	return roots
}

// scoopAppsMarker is the default Scoop layout; scoopRoots covers relocations.
const scoopAppsMarker = "/scoop/apps/"

// scoopAppName returns the Scoop app directory the binary runs from ("cli" or
// "entire"), or "" when execPath is not under a Scoop apps root. This is the
// durable signal for the pre-rename package: a binary running from the `cli`
// app dir must migrate to `entire` regardless of its version (the fix ships in
// a final `cli` release, so the migrating binary's version is already past the
// rename).
func scoopAppName(execPath string) string {
	for _, raw := range scoopRoots() {
		root := normalizeInstallRoot(raw)
		if below, ok := strings.CutPrefix(execPath, root); ok && root != "" {
			return firstPathSegment(below)
		}
	}
	if _, below, ok := strings.Cut(execPath, scoopAppsMarker); ok {
		return firstPathSegment(below)
	}
	return ""
}

func firstPathSegment(p string) string {
	app, _, _ := strings.Cut(p, "/")
	return app
}

func scoopUpgradeCommand(execPath, _ string) string {
	// The Scoop package was renamed from `cli` to `entire`. A binary still
	// running from the old `cli` app dir can never cross the rename with a
	// plain `scoop update entire/cli`, so migrate it: install the new
	// `entire` package, remove the old `cli` package, then reset the shared
	// shims. Both manifests declare `bin: [git-remote-entire.exe,
	// entire.exe]`, so the two packages contend for the same shims. Current
	// Scoop handles that itself: installing `entire` renames the displaced
	// shim to `entire.shim.cli` (warn_on_overwrite) and uninstalling `cli`
	// removes only that suffixed copy (rm_shim), leaving the active shims
	// pointing at `entire`. The reset step is belt-and-braces for older
	// Scoop versions that overwrote shims without that alt-file dance and
	// would otherwise take `entire.exe` and the git remote helper with them.
	//
	// The Windows updater is print-only, so return a self-contained command
	// users can paste into either cmd or PowerShell. Explicitly invoking
	// cmd.exe makes `&&` portable across those shells and ensures uninstall
	// only runs after install succeeds — a stale bucket or transient failure
	// therefore never removes the working CLI. (The uninstall→reset link is
	// weaker: `scoop uninstall` ends in an unconditional `exit 0`, so reset
	// runs even when the uninstall skipped its work. That is harmless, since
	// reset is a no-op on the Scoop versions that make it unnecessary.)
	//
	// `scoop install` also auto-refreshes the bucket when >3h stale
	// (is_scoop_outdated), so the new manifest usually lands
	// without an explicit refresh; a bucket clone older than that check can
	// still fail with "couldn't find manifest", and the README migration
	// section names `scoop update` as the retry. Binaries already on the
	// `entire` app just update in place.
	if scoopAppName(execPath) == "cli" {
		return `cmd.exe /D /C "scoop install entire/entire && scoop uninstall entire/cli && scoop reset entire"`
	}
	return "scoop update entire/entire"
}

var scoopProbe = installProbe{
	roots:   scoopRoots,
	markers: []string{scoopAppsMarker},
	command: scoopUpgradeCommand,
}

var installProbes = []installProbe{scoopProbe, miseProbe}

func fallbackInstallCommand(currentVersion string) string {
	if isNightly(currentVersion) {
		return windowsInstallNightlyCmd
	}
	return windowsInstallCmd
}
