//go:build windows

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// countOldEntries counts rename-aside leftovers in dir.
func countOldEntries(t *testing.T, dir string) int {
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

// Upgrading a plugin while its executable runs: MoveFileEx cannot replace a
// running image, so before the rename-aside fallback the bin\ swap failed with
// "Access is denied" and left the manifest ahead of the running binary.
func TestUpgrade_WindowsReplacesRunningPluginExecutable(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	// v0.1.0 must be a real executable that stays running; ping.exe ships
	// with every Windows install and waits ~1s per echo.
	ping, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE"))
	if err != nil {
		t.Skipf("no ping.exe to run as the plugin: %v", err)
	}
	srv := pluginReleaseServerWithPayloads(t, map[string][]byte{
		"0.1.0": ping,
		"0.2.0": []byte("payload-0.2.0"),
	})
	repoURL := newTaggedPluginRepo(t, fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL), remoteTestTagOld)
	res, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{})
	if err != nil {
		t.Fatalf("initial install: %v", err)
	}

	cmd := exec.CommandContext(ctx, res.Installed.Path, "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start installed plugin: %v", err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill() //nolint:errcheck // test cleanup
			_ = cmd.Wait()         //nolint:errcheck // killed on purpose
		}
	}
	t.Cleanup(stop)

	gitTag(t, repoURL, remoteTestTagMid)
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil {
		t.Fatalf("upgrade while the plugin runs: %v", err)
	}
	if o.ToTag != remoteTestTagMid {
		t.Errorf("upgrade outcome = %+v, want → %s", o, remoteTestTagMid)
	}
	if got := installedPayload(t); got != "payload-0.2.0" {
		t.Errorf("bin entry after upgrade = %d bytes, want payload-0.2.0", len(got))
	}
	installed, err := ListInstalledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].Name != "demo" {
		t.Errorf("ListInstalledPlugins = %+v, want only demo (no .old- entry)", installed)
	}
	binDir, err := PluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	// The running image is normally left behind as one .old- entry (it cannot
	// be deleted while mapped); it must never be more than that.
	if n := countOldEntries(t, binDir); n > 1 {
		t.Errorf("bin dir holds %d .old- entries while the old version runs, want at most 1", n)
	}

	// Once the old version exits, the next install sweeps its leftovers from
	// both bin\ and pkg\.
	stop()
	if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{Force: true}); err != nil {
		t.Fatalf("reinstall after exit: %v", err)
	}
	pkgDir, err := PluginPkgDir("demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{binDir, pkgDir} {
		if n := countOldEntries(t, dir); n != 0 {
			t.Errorf("%s still holds %d .old- entries after the next install", dir, n)
		}
	}
}
