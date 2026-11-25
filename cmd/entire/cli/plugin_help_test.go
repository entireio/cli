package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// helpPluginBin is the shape of a seeded plugin's bin/ entry.
type helpPluginBin int

const (
	binNone       helpPluginBin = iota // manifest only, nothing dispatchable
	binPkg                             // hardlink to pkg/<name>/entire-<name>, a release install's fallback
	binPkgSymlink                      // symlink to pkg/<name>/entire-<name>, what a release install leaves on Unix
	binOther                           // an unrelated file, as a local --force install over a release leaves it
)

// seedHelpPlugin writes a manifest and pkg/ binary for name, plus a bin/ entry
// of the given shape. Binaries are stubs that fail loudly: help must never run
// them.
func seedHelpPlugin(t *testing.T, pluginDir, name, description string, bin helpPluginBin) {
	t.Helper()
	if err := SavePluginManifest(&PluginManifest{
		Name:        name,
		RepoURL:     "https://example.com/entire-" + name,
		Tag:         "v1.0.0",
		Description: description,
	}); err != nil {
		t.Fatalf("SavePluginManifest(%s): %v", name, err)
	}
	stub := []byte("#!/bin/sh\nexit 99\n")
	pkgBin := filepath.Join(pluginDir, pluginManagedPkgSubdir, name, pluginBinaryName(name))
	if err := os.WriteFile(pkgBin, stub, 0o755); err != nil {
		t.Fatal(err)
	}
	if bin == binNone {
		return
	}
	binDir := filepath.Join(pluginDir, pluginManagedBinSubdir)
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(binDir, pluginBinaryName(name))
	if bin == binPkgSymlink {
		if err := os.Symlink(pkgBin, entry); err != nil {
			t.Fatal(err)
		}
		return
	}
	if bin == binPkg {
		if err := os.Link(pkgBin, entry); err != nil {
			t.Fatal(err)
		}
		return
	}
	// Same bytes on purpose: only identity, not content, may decide.
	if err := os.WriteFile(entry, stub, 0o755); err != nil {
		t.Fatal(err)
	}
}

func rootHelpOutput(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("entire %v: %v", args, err)
	}
	return out.String()
}

func TestRootHelp_ListsOnlyDescribedLinkedPlugins(t *testing.T) { //nolint:paralleltest // mutates ENTIRE_PLUGIN_DIR
	dir := withPluginDir(t)
	seedHelpPlugin(t, dir, "zz-valid", "Does valid things", binPkg)
	seedHelpPlugin(t, dir, "aa-valid", "Comes first by name", binPkg)
	seedHelpPlugin(t, dir, "nodesc", "", binPkg)
	seedHelpPlugin(t, dir, "blank", "   ", binPkg)
	seedHelpPlugin(t, dir, "status", "Shadowed by the built-in", binPkg)
	seedHelpPlugin(t, dir, "cp", "Shadowed by a built-in alias", binPkg)
	seedHelpPlugin(t, dir, "ctrl", "evil\x1b[2Jrepaint", binPkg)
	seedHelpPlugin(t, dir, "bidi", "spoof\u202e", binPkg)
	seedHelpPlugin(t, dir, "long", strings.Repeat("x", maxPluginHelpDescriptionLen+1), binPkg)
	seedHelpPlugin(t, dir, "nolink", "Manifest without a bin entry", binNone)
	seedHelpPlugin(t, dir, "localdev", "Stale manifest under a local build", binOther)

	for _, args := range [][]string{{"--help"}, {"help"}} {
		out := rootHelpOutput(t, args...)
		section := pluginHelpSection(t, out)
		lines := strings.Split(strings.TrimSpace(section), "\n")
		if len(lines) != 2 {
			t.Fatalf("entire %v: plugin section = %q, want exactly the two valid plugins", args, section)
		}
		if !strings.HasPrefix(strings.TrimSpace(lines[0]), "aa-valid") || !strings.HasSuffix(lines[0], " Comes first by name") {
			t.Errorf("entire %v: first plugin line = %q, want aa-valid sorted first", args, lines[0])
		}
		if !strings.HasPrefix(strings.TrimSpace(lines[1]), "zz-valid") || !strings.HasSuffix(lines[1], " Does valid things") {
			t.Errorf("entire %v: second plugin line = %q", args, lines[1])
		}
		for _, hidden := range []string{"nodesc", "blank", "Shadowed", "evil", "spoof", "xxxx", "nolink", "Stale"} {
			if strings.Contains(section, hidden) {
				t.Errorf("entire %v: plugin section lists %q:\n%s", args, hidden, section)
			}
		}
	}
}

// pluginHelpSection returns the lines between the "Plugin Commands:" header and
// the next section, failing when the header is absent or misplaced.
func pluginHelpSection(t *testing.T, out string) string {
	t.Helper()
	const header = "\nPlugin Commands:\n"
	start := strings.Index(out, header)
	if start < 0 {
		t.Fatalf("root help has no Plugin Commands section:\n%s", out)
	}
	if addl := strings.Index(out, "\nAdditional Commands:\n"); addl < 0 || addl > start {
		t.Errorf("Plugin Commands should follow the built-in command groups:\n%s", out)
	}
	flags := strings.Index(out, "\nFlags:\n")
	if flags < start {
		t.Fatalf("Plugin Commands should precede Flags:\n%s", out)
	}
	return out[start+len(header) : flags]
}

// A release install on Unix links bin/ to pkg/ with a symlink first, so the
// identity check must follow it. Pins that against a later switch to Lstat or
// a no-symlink read, which would silently empty the section.
func TestRootHelp_ListsSymlinkedReleaseInstall(t *testing.T) { //nolint:paralleltest // mutates ENTIRE_PLUGIN_DIR
	if runtime.GOOS == windowsGOOS {
		t.Skip("release installs never symlink on Windows (see plugin_store_windows.go)")
	}
	dir := withPluginDir(t)
	seedHelpPlugin(t, dir, "linked", "Symlinked release install", binPkgSymlink)
	section := pluginHelpSection(t, rootHelpOutput(t, "--help"))
	if !strings.Contains(section, "linked") || !strings.Contains(section, "Symlinked release install") {
		t.Errorf("symlinked release install not listed:\n%s", section)
	}
}

func TestRootHelp_NoPluginSectionWithoutDescribedPlugins(t *testing.T) { //nolint:paralleltest // mutates ENTIRE_PLUGIN_DIR
	dir := withPluginDir(t)
	seedHelpPlugin(t, dir, "nodesc", "", binPkg)
	if out := rootHelpOutput(t, "--help"); strings.Contains(out, "Plugin Commands") {
		t.Errorf("root help shows an empty Plugin Commands section:\n%s", out)
	}
}

func TestSubcommandHelp_OmitsPluginSection(t *testing.T) { //nolint:paralleltest // mutates ENTIRE_PLUGIN_DIR
	dir := withPluginDir(t)
	seedHelpPlugin(t, dir, "valid", "Does valid things", binPkg)
	for _, args := range [][]string{{"plugin", "--help"}, {"help", "status"}} {
		out := rootHelpOutput(t, args...)
		if strings.Contains(out, "Plugin Commands") || strings.Contains(out, "Does valid things") {
			t.Errorf("entire %v lists plugins; only root help should:\n%s", args, out)
		}
		if !strings.Contains(out, "Usage:") {
			t.Errorf("entire %v lost its usage section:\n%s", args, out)
		}
	}
}

func TestInstallPluginFromRepo_CopiesDescription(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	srv := pluginReleaseServer(t, "0.1.0")
	meta := fmt.Sprintf("name: demo\ndescription: Demo plugin for tests\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)

	res, err := InstallPluginFromRepo(context.Background(), repoURL, "", RemoteInstallOptions{})
	if err != nil {
		t.Fatalf("InstallPluginFromRepo: %v", err)
	}
	if res.Manifest.Description != "Demo plugin for tests" {
		t.Errorf("result manifest description = %q", res.Manifest.Description)
	}
	saved, err := LoadPluginManifest("demo")
	if err != nil || saved == nil {
		t.Fatalf("LoadPluginManifest: %v, %v", saved, err)
	}
	if saved.Description != "Demo plugin for tests" {
		t.Errorf("saved manifest description = %q, want it copied from %s", saved.Description, pluginMetadataFileName)
	}
}

// stripManifestDescription makes an install look like one made before
// manifests recorded descriptions.
func stripManifestDescription(t *testing.T, name string) {
	t.Helper()
	m, err := LoadPluginManifest(name)
	if err != nil || m == nil {
		t.Fatalf("LoadPluginManifest(%s) = %v, %v", name, m, err)
	}
	m.Description = ""
	if err := SavePluginManifest(m); err != nil {
		t.Fatal(err)
	}
}

func savedDescription(t *testing.T, name string) string {
	t.Helper()
	m, err := LoadPluginManifest(name)
	if err != nil || m == nil {
		t.Fatalf("LoadPluginManifest(%s) = %v, %v", name, m, err)
	}
	return m.Description
}

// An install made before descriptions were recorded must pick one up on its
// next upgrade even when there is nothing to install: up to date, or pinned.
// Those are the two branches that return before reinstalling.
func TestUpgradeInstalledPlugin_BackfillsDescriptionWithoutReinstall(t *testing.T) { //nolint:paralleltest // mutates env
	for _, pin := range []string{"", remoteTestTagOld} {
		t.Run("pin="+pin, func(t *testing.T) {
			withIsolatedPluginEnv(t)
			ctx := context.Background()
			srv := pluginReleaseServer(t, "0.1.0")
			meta := fmt.Sprintf("name: demo\ndescription: Demo plugin for tests\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
			repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)
			if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{Pin: pin}); err != nil {
				t.Fatalf("InstallPluginFromRepo: %v", err)
			}
			stripManifestDescription(t, "demo")

			o, err := UpgradeInstalledPlugin(ctx, "demo")
			if err != nil {
				t.Fatalf("UpgradeInstalledPlugin: %v", err)
			}
			if pin != "" && !o.Pinned {
				t.Errorf("outcome = %+v, want Pinned", o)
			}
			if pin == "" && !o.UpToDate {
				t.Errorf("outcome = %+v, want UpToDate", o)
			}
			if got := savedDescription(t, "demo"); got != "Demo plugin for tests" {
				t.Errorf("description after upgrade = %q, want it backfilled from %s", got, pluginMetadataFileName)
			}
			if got := installedPayload(t); got != "payload-0.1.0" {
				t.Errorf("payload = %q; backfill must not reinstall", got)
			}
		})
	}
}

// Backfill is best effort: metadata without a description, or a repository
// that can no longer be reached, leaves the description empty and the
// upgrade outcome unchanged.
func TestUpgradeInstalledPlugin_BackfillIsBestEffort(t *testing.T) { //nolint:paralleltest // mutates env
	cases := map[string]struct {
		description string
		unreachable bool
	}{
		"metadata without description": {},
		"repository gone":              {description: "Demo plugin for tests", unreachable: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			withIsolatedPluginEnv(t)
			ctx := context.Background()
			srv := pluginReleaseServer(t, "0.1.0")
			meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
			if tc.description != "" {
				meta = "description: " + tc.description + "\n" + meta
			}
			repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)
			if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{Pin: remoteTestTagOld}); err != nil {
				t.Fatalf("InstallPluginFromRepo: %v", err)
			}
			stripManifestDescription(t, "demo")
			if tc.unreachable {
				if err := os.RemoveAll(repoDirFromURL(repoURL)); err != nil {
					t.Fatal(err)
				}
			}

			o, err := UpgradeInstalledPlugin(ctx, "demo")
			if err != nil || !o.Pinned {
				t.Fatalf("UpgradeInstalledPlugin = %+v, %v; want a clean Pinned skip", o, err)
			}
			if got := savedDescription(t, "demo"); got != "" {
				t.Errorf("description = %q, want it left empty", got)
			}
		})
	}
}
