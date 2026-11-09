package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errSimulatedAccessDenied stands in for the sharing violation MoveFileEx
// returns when the rename target is a running executable.
var errSimulatedAccessDenied = errors.New("access is denied")

// simulateRunningManagedEntry makes managedEntryRename behave like Windows
// with destName's current file running: renaming onto it fails, renaming it
// away succeeds (and the "running" file moves with it). asideFallback sets
// renameAsideOnReplaceFailure, so the same simulation covers the fixed and the
// unfixed behaviour on every platform.
func simulateRunningManagedEntry(t *testing.T, destName string, asideFallback bool) {
	t.Helper()
	held := destName
	setManagedEntryRenameSeam(t, asideFallback, func(root *os.Root, from, to string) error {
		if to == held {
			return &os.LinkError{Op: "renameat", Old: from, New: to, Err: errSimulatedAccessDenied}
		}
		if err := root.Rename(from, to); err != nil {
			return err
		}
		if from == held {
			held = to
		}
		return nil
	})
}

func setManagedEntryRenameSeam(t *testing.T, asideFallback bool, rename func(*os.Root, string, string) error) {
	t.Helper()
	prevAside, prevRename := renameAsideOnReplaceFailure, managedEntryRename
	renameAsideOnReplaceFailure, managedEntryRename = asideFallback, rename
	t.Cleanup(func() { renameAsideOnReplaceFailure, managedEntryRename = prevAside, prevRename })
}

// writePluginSource writes an entire-foo executable with body into its own
// temp dir and returns its path.
func writePluginSource(t *testing.T, body string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), pluginBinaryName("foo"))
	writeExecutableScript(t, src, body)
	return src
}

func binDirEntries(t *testing.T) []string {
	t.Helper()
	binDir, err := PluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readInstalledFoo(t *testing.T) string {
	t.Helper()
	p, err := FindInstalledPlugin("foo")
	if err != nil || p == nil {
		t.Fatalf("FindInstalledPlugin(foo) = %v, %v", p, err)
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A running Windows executable cannot be replaced by rename, only renamed.
// The bin/ swap must move it aside and retry, and the aside file must be
// invisible to listing and dispatch.
func TestInstallPluginFromPath_ReplacesRunningEntryByMovingItAside(t *testing.T) { //nolint:paralleltest // mutates env and seams
	withPluginDir(t)
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	simulateRunningManagedEntry(t, pluginBinName(pluginBinaryName("foo")), true)

	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true}); err != nil {
		t.Fatalf("replace running entry: %v", err)
	}
	if got := readInstalledFoo(t); got != "new" {
		t.Errorf("installed entry = %q, want new", got)
	}
	installed, err := ListInstalledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].Name != "foo" {
		t.Errorf("ListInstalledPlugins = %+v, want only foo", installed)
	}
}

// Without the fallback the simulation reproduces the reported failure, which
// is what keeps the test above honest.
func TestInstallPluginFromPath_RunningEntryFailsWithoutAsideFallback(t *testing.T) { //nolint:paralleltest // mutates env and seams
	withPluginDir(t)
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	simulateRunningManagedEntry(t, pluginBinName(pluginBinaryName("foo")), false)

	_, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true})
	if !errors.Is(err, errSimulatedAccessDenied) {
		t.Fatalf("replace without fallback = %v, want access denied", err)
	}
	if got := readInstalledFoo(t); got != "old" {
		t.Errorf("installed entry = %q after a failed replace, want old", got)
	}
}

// If the retry fails too, the moved-aside entry goes back where it was: a
// failed replace must never leave the plugin missing.
func TestInstallPluginFromPath_RestoresEntryWhenRetryFails(t *testing.T) { //nolint:paralleltest // mutates env and seams
	withPluginDir(t)
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	destName := pluginBinName(pluginBinaryName("foo"))
	setManagedEntryRenameSeam(t, true, func(root *os.Root, from, to string) error {
		if to == destName && strings.Contains(from, ".install-") {
			return &os.LinkError{Op: "renameat", Old: from, New: to, Err: errSimulatedAccessDenied}
		}
		return root.Rename(from, to)
	})

	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true}); err == nil {
		t.Fatal("replace succeeded although every swap onto the entry failed")
	}
	if got := readInstalledFoo(t); got != "old" {
		t.Errorf("installed entry = %q, want the previous one restored", got)
	}
	if names := binDirEntries(t); len(names) != 1 {
		t.Errorf("bin dir = %v, want only the restored entry", names)
	}
}

// Aside leftovers survive while their process runs; the next install sweeps
// them, and until then they never list as plugins.
func TestInstallPluginFromPath_SweepsAndHidesAsideLeftovers(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	binDir, err := PluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	leftover, err := oldManagedEntryName(pluginBinaryName("foo"))
	if err != nil {
		t.Fatal(err)
	}
	leftoverPath := filepath.Join(binDir, filepath.Base(leftover))
	writeExecutableScript(t, leftoverPath, "stale")

	installed, err := ListInstalledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].Name != "foo" {
		t.Errorf("ListInstalledPlugins with a leftover = %+v, want only foo", installed)
	}

	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if _, err := os.Lstat(leftoverPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("leftover %s not swept: %v", leftoverPath, err)
	}
}

// A failed bin/ swap must not advance pkg/ and the manifest: otherwise the
// manifest records the new tag over the old running binary and every later
// upgrade reports "up to date". The next upgrade has to retry and succeed.
func TestUpgrade_FailedBinSwapLeavesPreviousVersionAndRetries(t *testing.T) { //nolint:paralleltest // mutates env and seams
	withIsolatedPluginEnv(t)
	ctx := context.Background()
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0", "0.2.0")
	if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("initial install: %v", err)
	}
	gitTag(t, repoURL, remoteTestTagMid)

	simulateRunningManagedEntry(t, pluginBinName(pluginBinaryName("demo")), false)
	if _, err := UpgradeInstalledPlugin(ctx, "demo"); !errors.Is(err, errSimulatedAccessDenied) {
		t.Fatalf("upgrade with a failing bin swap = %v, want access denied", err)
	}

	m, err := LoadPluginManifest("demo")
	if err != nil || m == nil {
		t.Fatalf("LoadPluginManifest = %v, %v", m, err)
	}
	if m.Tag != remoteTestTagOld {
		t.Errorf("manifest tag = %s after a failed upgrade, want %s", m.Tag, remoteTestTagOld)
	}
	if got := installedPayload(t); got != "payload-0.1.0" {
		t.Errorf("bin entry payload = %q, want payload-0.1.0", got)
	}
	pkgDir, err := PluginPkgDir("demo")
	if err != nil {
		t.Fatal(err)
	}
	pkgEntries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgEntries) != 2 {
		t.Errorf("pkg dir holds %d entries after rollback, want binary + manifest", len(pkgEntries))
	}
	if digest, err := fileSHA256(filepath.Join(pkgDir, pluginBinaryName("demo"))); err != nil || digest != m.BinarySHA256 {
		t.Errorf("pkg binary digest = %s (%v), manifest binary_sha256 = %s", digest, err, m.BinarySHA256)
	}
	issues, err := RunPluginDoctor(ctx)
	if err != nil || len(issues) != 0 {
		t.Errorf("doctor after rollback: issues=%+v err=%v", issues, err)
	}

	// The process exits; the next upgrade must see the new tag as pending.
	setManagedEntryRenameSeam(t, false, func(root *os.Root, from, to string) error {
		return root.Rename(from, to)
	})
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil {
		t.Fatalf("retry upgrade: %v", err)
	}
	if o.UpToDate || o.ToTag != remoteTestTagMid {
		t.Errorf("retry upgrade outcome = %+v, want %s → %s", o, remoteTestTagOld, remoteTestTagMid)
	}
	if got := installedPayload(t); got != "payload-0.2.0" {
		t.Errorf("bin entry payload after retry = %q, want payload-0.2.0", got)
	}
}

// A failed first install leaves no manifest behind claiming it succeeded.
func TestInstallPluginFromRepo_FailedBinSwapLeavesNoManifest(t *testing.T) { //nolint:paralleltest // mutates env and seams
	withIsolatedPluginEnv(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	setManagedEntryRenameSeam(t, false, func(_ *os.Root, from, to string) error {
		return &os.LinkError{Op: "renameat", Old: from, New: to, Err: errSimulatedAccessDenied}
	})
	if _, err := InstallPluginFromRepo(context.Background(), repoURL, "", RemoteInstallOptions{}); err == nil {
		t.Fatal("install succeeded although the bin swap failed")
	}
	if m, err := LoadPluginManifest("demo"); err != nil || m != nil {
		t.Errorf("LoadPluginManifest after failed install = %+v, %v; want none", m, err)
	}
	if p, err := FindInstalledPlugin("demo"); err != nil || p != nil {
		t.Errorf("FindInstalledPlugin after failed install = %+v, %v; want none", p, err)
	}
}
