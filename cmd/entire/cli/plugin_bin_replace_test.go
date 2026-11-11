package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	simulateRunningManagedEntry(t, pluginBinName(pluginBinaryName("foo")), true)

	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true}); err != nil {
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
	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	simulateRunningManagedEntry(t, pluginBinName(pluginBinaryName("foo")), false)

	_, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true})
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
	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	destName := pluginBinName(pluginBinaryName("foo"))
	setManagedEntryRenameSeam(t, true, func(root *os.Root, from, to string) error {
		if to == destName && strings.Contains(from, ".install-") {
			return &os.LinkError{Op: "renameat", Old: from, New: to, Err: errSimulatedAccessDenied}
		}
		return root.Rename(from, to)
	})

	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true}); err == nil {
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
	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	binDir, err := PluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	leftover := oldManagedEntryName(pluginBinaryName("foo"))
	leftoverPath := filepath.Join(binDir, filepath.Base(leftover))
	writeExecutableScript(t, leftoverPath, "stale")

	installed, err := ListInstalledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].Name != "foo" {
		t.Errorf("ListInstalledPlugins with a leftover = %+v, want only foo", installed)
	}

	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "new"), Force: true}); err != nil {
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
	assertFailedBinSwapRollsBack(t)
}

// The same, where the previous pkg binary cannot be hard-linked aside and is
// moved instead (filesystems without hard links).
func TestUpgrade_FailedBinSwapRollsBackWithoutHardLinks(t *testing.T) { //nolint:paralleltest // mutates env and seams
	prev := pkgBackupLink
	pkgBackupLink = func(*os.Root, string, string) error { return errors.New("hard links unsupported") }
	t.Cleanup(func() { pkgBackupLink = prev })
	assertFailedBinSwapRollsBack(t)
}

func assertFailedBinSwapRollsBack(t *testing.T) {
	t.Helper()
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
	if entries, err := os.ReadDir(pkgDir); err != nil || len(entries) != 2 {
		t.Errorf("pkg dir after a successful upgrade holds %d entries (%v), want binary + manifest", len(entries), err)
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

// `plugin remove` is the last install-tree change a plugin sees, so it must
// sweep the entries an upgrade moved aside itself; nothing later would.
func TestRemoveManagedPlugin_SweepsAsideLeftovers(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatalf("install: %v", err)
	}
	binDir, err := PluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	leftoverPath := filepath.Join(binDir, filepath.Base(oldManagedEntryName(pluginBinaryName("foo"))))
	writeExecutableScript(t, leftoverPath, "stale")

	if err := RemoveManagedPlugin(t.Context(), "foo"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Lstat(leftoverPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("leftover %s survived remove: %v", leftoverPath, err)
	}
}

// pluginTreeLockHeld reports whether another holder has the plugin tree lock.
func pluginTreeLockHeld(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	release, err := lockPluginTree(ctx, false)
	if err != nil {
		return true
	}
	release()
	return false
}

// An install holds the tree lock through the bin/ swap, so a concurrent
// install's sweep cannot delete the pkg/ backup it would roll back from.
func TestUpgrade_HoldsPluginTreeLockThroughBinSwap(t *testing.T) { //nolint:paralleltest // mutates env and seams
	withIsolatedPluginEnv(t)
	ctx := context.Background()
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0", "0.2.0")
	if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("initial install: %v", err)
	}
	gitTag(t, repoURL, remoteTestTagMid)

	heldDuringSwap := false
	setManagedEntryRenameSeam(t, false, func(root *os.Root, from, to string) error {
		heldDuringSwap = pluginTreeLockHeld(t)
		return root.Rename(from, to)
	})
	if _, err := UpgradeInstalledPlugin(ctx, "demo"); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !heldDuringSwap {
		t.Error("plugin tree lock was free during the bin/ swap")
	}
	if pluginTreeLockHeld(t) {
		t.Error("plugin tree lock still held after the upgrade returned")
	}
}

// A local install waits for whoever holds the tree lock.
func TestInstallPluginFromPath_WaitsForPluginTreeLock(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	release, err := lockPluginTree(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	src := writePluginSource(t, "new")
	done := make(chan error, 1)
	go func() {
		_, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: src})
		done <- err
	}()
	select {
	case err := <-done:
		release()
		t.Fatalf("install finished (%v) while the tree lock was held", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("install after release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("install still blocked after the lock was released")
	}
}

// Ctrl-C (a cancelled ctx with no deadline, like main's root ctx) stops a wait
// for the tree lock instead of blocking until the other holder finishes.
func TestLockPluginTree_CancelStopsTheWait(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	release, err := lockPluginTree(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		rel, err := lockPluginTree(ctx, true)
		if err == nil {
			rel()
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("lockPluginTree after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lockPluginTree ignored the cancelled ctx while another holder had the lock")
	}
}

// When the install fails before replacing the pkg binary, restore keeps that
// binary and drops the hard-linked backup instead of renaming it back on top:
// on Windows that rename fails while the binary runs, and on Unix it is a
// no-op that leaves the .old- link behind.
func TestPkgInstallBackup_RestoreWithUntouchedLinkedBinary(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if _, err := EnsurePluginPkgDir("foo"); err != nil {
		t.Fatal(err)
	}
	root, err := pluginRoot(true)
	if err != nil {
		t.Fatal(err)
	}
	pkgName, err := pluginPkgName("foo")
	if err != nil {
		t.Fatal(err)
	}
	binBase := pluginBinaryName("foo")
	if err := writeExecutable(root, pkgName+"/"+binBase, strings.NewReader("previous")); err != nil {
		t.Fatal(err)
	}
	b, err := stashPreviousPkgInstall(root, pkgName, binBase)
	if err != nil {
		t.Fatal(err)
	}
	if !b.linked {
		t.Skip("filesystem without hard links; the moved-aside path is covered elsewhere")
	}
	if err := b.restore(); err != nil {
		t.Fatalf("restore with an untouched binary = %v, want nil", err)
	}
	data, err := root.ReadFile(b.binName)
	if err != nil || string(data) != "previous" {
		t.Errorf("pkg binary after restore = %q (%v), want previous", data, err)
	}
	pkgDir, err := PluginPkgDir("foo")
	if err != nil {
		t.Fatal(err)
	}
	if n := countAsideEntries(t, pkgDir); n != 0 {
		t.Errorf("pkg dir holds %d .old- entries after restore, want 0", n)
	}
}

func countAsideEntries(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old-") {
			n++
		}
	}
	return n
}

const pluginTreeWaitNotice = "Waiting for another plugin install to finish..."

// `plugin install ./path` and `plugin remove` stuck behind another holder of
// the tree lock say so once, and the first Ctrl-C (a cancelled command ctx)
// ends the wait with an error main.go treats as an interrupt.
func TestPluginTreeLockWait_AnnouncedAndCancellable(t *testing.T) { //nolint:paralleltest // mutates env
	cases := []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"install from path", func(ctx context.Context) error {
			_, err := InstallPluginFromPath(ctx, InstallPluginOptions{SourcePath: writePluginSource(t, "new")})
			return err
		}},
		{"remove", func(ctx context.Context) error { return RemoveManagedPlugin(ctx, "foo") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest // mutates env
			withPluginDir(t)
			release, err := lockPluginTree(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			defer release()

			var progress bytes.Buffer
			ctx, cancel := context.WithCancel(withPluginProgress(context.Background(), &progress))
			done := make(chan error, 1)
			go func() { done <- tc.run(ctx) }()
			time.Sleep(150 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s while the tree lock is held, then cancelled = %v, want context.Canceled", tc.name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s ignored the cancelled ctx while waiting for the tree lock", tc.name)
			}
			if n := strings.Count(progress.String(), pluginTreeWaitNotice); n != 1 {
				t.Errorf("wait notice written %d times, want once; progress = %q", n, progress.String())
			}
		})
	}
}

// Taking a free tree lock says nothing.
func TestPluginTreeLock_UncontendedIsSilent(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	var progress bytes.Buffer
	if _, err := InstallPluginFromPath(withPluginProgress(context.Background(), &progress), InstallPluginOptions{SourcePath: writePluginSource(t, "new")}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if progress.Len() != 0 {
		t.Errorf("uncontended install wrote progress %q, want nothing", progress.String())
	}
}
