package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pluginLockWaitNotice starts every lock-wait notice, the tree's and each
// plugin's.
const pluginLockWaitNotice = "Waiting for another "

// waitNoticeWriter is a plugin progress writer that reports when the
// lock-wait notice is written, so a test knows its goroutine is blocked on a
// plugin lock without sleeping.
type waitNoticeWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
	seen chan struct{}
}

func newWaitNoticeWriter() *waitNoticeWriter {
	return &waitNoticeWriter{seen: make(chan struct{})}
}

func (w *waitNoticeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), pluginLockWaitNotice) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *waitNoticeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// awaitWaiting blocks until w's goroutine is waiting on a plugin lock.
func (w *waitNoticeWriter) awaitWaiting(t *testing.T) {
	t.Helper()
	select {
	case <-w.seen:
	case <-time.After(30 * time.Second):
		t.Fatal("the operation never started waiting for the plugin lock")
	}
}

// holdPluginLock takes name's plugin lock for the test, as a concurrent
// command would, and returns its release (also run at cleanup).
func holdPluginLock(t *testing.T, name string) func() {
	t.Helper()
	release, err := lockPlugin(context.Background(), name, true)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	rel := func() { once.Do(release) }
	t.Cleanup(rel)
	return rel
}

// receive waits for an operation's result.
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(60 * time.Second):
		t.Fatal("the operation did not finish after the plugin lock was released")
	}
	var zero T
	return zero
}

type upgradeResult struct {
	outcome *UpgradeOutcome
	err     error
}

// installDemoForUpgrade installs demo at v0.1.0 with v0.2.0 tagged after it,
// so an upgrade has something to do.
func installDemoForUpgrade(t *testing.T) {
	t.Helper()
	withIsolatedPluginEnv(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0", "0.2.0")
	if _, err := InstallPluginFromRepo(t.Context(), repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("initial install: %v", err)
	}
	gitTag(t, repoURL, remoteTestTagMid)
}

// startUpgrade runs UpgradeInstalledPlugin(demo) in the background once demo's
// lock is held, returning once it is waiting for that lock.
func startUpgrade(t *testing.T) <-chan upgradeResult {
	t.Helper()
	w := newWaitNoticeWriter()
	done := make(chan upgradeResult, 1)
	go func() {
		o, err := UpgradeInstalledPlugin(withPluginProgress(t.Context(), w), "demo")
		done <- upgradeResult{o, err}
	}()
	w.awaitWaiting(t)
	return done
}

// An upgrade that was waiting while `plugin remove` finished must not
// reinstall the removed plugin: it decides under the lock, after the remove.
func TestUpgrade_AfterConcurrentRemoveReportsNotInstalled(t *testing.T) { //nolint:paralleltest // mutates env
	installDemoForUpgrade(t)
	release := holdPluginLock(t, "demo")
	done := startUpgrade(t)

	// The remove, done while holding demo's lock as `plugin remove` would.
	if err := RemoveInstalledPlugin("demo"); err != nil {
		t.Fatal(err)
	}
	if err := RemovePluginPkg("demo"); err != nil {
		t.Fatal(err)
	}
	release()

	res := receive(t, done)
	if res.err == nil || !strings.Contains(res.err.Error(), "not installed") {
		t.Errorf("upgrade after a concurrent remove = (%+v, %v), want not installed", res.outcome, res.err)
	}
	if p, err := FindInstalledPlugin("demo"); err != nil || p != nil {
		t.Errorf("demo after the upgrade = %+v (%v), want still removed", p, err)
	}
}

// An upgrade that was waiting while `plugin install --pin` finished must
// honour the pin rather than overwrite it with the latest tag.
func TestUpgrade_AfterConcurrentPinReportsPinned(t *testing.T) { //nolint:paralleltest // mutates env
	installDemoForUpgrade(t)
	release := holdPluginLock(t, "demo")
	done := startUpgrade(t)

	m, err := LoadPluginManifest("demo")
	if err != nil || m == nil {
		t.Fatalf("LoadPluginManifest = %v, %v", m, err)
	}
	m.Pinned = true
	if err := SavePluginManifest(m); err != nil {
		t.Fatal(err)
	}
	release()

	res := receive(t, done)
	if res.err != nil || res.outcome == nil || !res.outcome.Pinned {
		t.Errorf("upgrade after a concurrent pin = (%+v, %v), want a Pinned outcome", res.outcome, res.err)
	}
	after, err := LoadPluginManifest("demo")
	if err != nil || after == nil || !after.Pinned || after.Tag != remoteTestTagOld {
		t.Errorf("manifest after the upgrade = %+v (%v), want still pinned at %s", after, err, remoteTestTagOld)
	}
}

// Two non-force installs of one name: the second, waiting while the first
// completed, sees the result and refuses rather than silently replacing it.
func TestInstallPluginFromRepo_WaitingNonForceInstallSeesTheFirst(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	repoA, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	repoB, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	release := holdPluginLock(t, "demo")

	w := newWaitNoticeWriter()
	done := make(chan error, 1)
	go func() {
		_, err := InstallPluginFromRepo(withPluginProgress(t.Context(), w), repoB, "", RemoteInstallOptions{})
		done <- err
	}()
	w.awaitWaiting(t)

	// The first install, holding the lock it was granted.
	if _, err := InstallPluginFromRepo(t.Context(), repoA, "demo", RemoteInstallOptions{heldPluginLock: "demo"}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	release()

	if err := receive(t, done); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Errorf("second non-force install = %v, want already installed", err)
	}
	m, err := LoadPluginManifest("demo")
	if err != nil || m == nil || normalizeRepoURL(m.RepoURL) != normalizeRepoURL(repoA) {
		t.Errorf("manifest = %+v (%v), want the first install's repo", m, err)
	}
}

// Plugins lock independently: one held does not hold up another.
func TestPluginLock_OtherPluginsDoNotWait(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	holdPluginLock(t, "bar")
	var progress bytes.Buffer
	ctx, cancel := context.WithTimeout(withPluginProgress(context.Background(), &progress), 10*time.Second)
	defer cancel()
	if _, err := InstallPluginFromPath(ctx, InstallPluginOptions{SourcePath: writePluginSource(t, "new")}); err != nil {
		t.Fatalf("install foo while bar is locked: %v", err)
	}
	if strings.Contains(progress.String(), pluginLockWaitNotice) {
		t.Errorf("install of foo waited on bar's lock; progress = %q", progress.String())
	}
}

// Ctrl-C while waiting for a plugin's lock ends the wait promptly.
func TestPluginLock_CancelStopsTheWait(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: writePluginSource(t, "old")}); err != nil {
		t.Fatal(err)
	}
	holdPluginLock(t, "foo")
	w := newWaitNoticeWriter()
	ctx, cancel := context.WithCancel(withPluginProgress(context.Background(), w))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := UpgradeInstalledPlugin(ctx, "foo")
		done <- err
	}()
	w.awaitWaiting(t)
	if got := w.String(); !strings.Contains(got, `plugin "foo"`) {
		t.Errorf("wait notice %q does not name the plugin waited on", got)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("upgrade cancelled while waiting = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade ignored the cancelled ctx while waiting for the plugin lock")
	}
}

// Upgrading or removing a name that is not installed takes no lock, so it
// leaves no lock file behind.
func TestPluginLock_AbsentPluginLeavesNoLockFile(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	holdPluginLock(t, "other") // the plugin tree and its locks dir exist
	if _, err := UpgradeInstalledPlugin(t.Context(), "ghost"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("upgrade ghost = %v, want not installed", err)
	}
	if err := RemoveManagedPlugin(t.Context(), "ghost"); err == nil {
		t.Error("remove ghost succeeded, want not installed")
	}
	dir, err := pluginParentDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, pluginLocksDir, "ghost.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ghost.lock after upgrade/remove of an absent plugin: %v, want none", err)
	}
}
