package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// setupRemoteRepo creates an isolated repo dir with the given settings and an
// empty (throwaway) cache, then chdirs into it. Returns the cache-file path so a
// test can assert whether an inline fetch wrote it.
func setupRemoteRepo(t *testing.T, settingsJSON string) (cacheFile string) {
	t.Helper()
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Chdir(dir)
	return filepath.Join(cacheHome, "entire", "pricing_remote.json")
}

// stubSpawn replaces the detached spawn hook with a counter for the test.
func stubSpawn(t *testing.T) *int {
	t.Helper()
	var calls int
	orig := spawnDetachedRefresh
	spawnDetachedRefresh = func() { calls++ }
	t.Cleanup(func() { spawnDetachedRefresh = orig })
	return &calls
}

// TestSpawnDetachedRefreshUsesProjectDir is the regression for the defect where
// the detached worker ran with cwd "/" (SpawnDetached's old hardcoded dir) and so
// could not see a project-level pricing.remote opt-in — the auto-refresh silently
// no-oped forever. The worker must be spawned rooted at the process working
// directory (the project the hook ran in).
func TestSpawnDetachedRefreshUsesProjectDir(t *testing.T) {
	// Not parallel: uses t.Chdir and swaps a package var.
	project := t.TempDir()
	resolved, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatalf("evalsymlinks(project): %v", err)
	}
	t.Chdir(resolved)

	var gotDir string
	spawned := false
	orig := detachedSpawn
	detachedSpawn = func(dir, _ string, _ ...string) error {
		gotDir = dir
		spawned = true
		return nil
	}
	t.Cleanup(func() { detachedSpawn = orig })

	spawnDetachedRefresh()

	if !spawned {
		t.Fatal("spawnDetachedRefresh did not spawn the worker")
	}
	if gotDir == "/" || gotDir == "" {
		t.Fatalf("worker dir = %q, want the project dir (a %q/empty dir hides project settings)", gotDir, "/")
	}
	gotResolved, err := filepath.EvalSymlinks(gotDir)
	if err != nil {
		t.Fatalf("evalsymlinks(gotDir=%q): %v", gotDir, err)
	}
	if gotResolved != resolved {
		t.Fatalf("worker dir = %q (resolved %q), want project dir %q", gotDir, gotResolved, resolved)
	}
}

func TestNewRefreshPricingCmd_Hidden(t *testing.T) {
	t.Parallel()
	cmd := newRefreshPricingCmd()
	if cmd.Use != refreshPricingCmdName {
		t.Errorf("Use = %q, want %q", cmd.Use, refreshPricingCmdName)
	}
	if !cmd.Hidden {
		t.Error("refresh-pricing worker command must be Hidden")
	}
}

func TestRefreshPricingCmd_RegisteredAndHidden(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Use == refreshPricingCmdName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatalf("%q not registered on root", refreshPricingCmdName)
	}
	if !found.Hidden {
		t.Errorf("%q must be Hidden so PersistentPostRun skips it", refreshPricingCmdName)
	}
}

func TestMaybeSpawnPricingRefresh_SpawnsWhenEnabledAndStale(t *testing.T) {
	cacheFile := setupRemoteRepo(t, `{"enabled":true,"pricing":{"remote":true}}`)
	calls := stubSpawn(t)

	maybeSpawnPricingRefresh(context.Background())

	if *calls != 1 {
		t.Errorf("spawn calls = %d, want 1 (enabled + no cache = stale)", *calls)
	}
	// The trigger must never fetch inline: no cache file should have been written.
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Errorf("cache file exists (%v); the trigger must only spawn, never fetch inline", err)
	}
}

func TestMaybeSpawnPricingRefresh_SkipsWhenDisabled(t *testing.T) {
	setupRemoteRepo(t, `{"enabled":true}`)
	calls := stubSpawn(t)

	maybeSpawnPricingRefresh(context.Background())

	if *calls != 0 {
		t.Errorf("spawn calls = %d, want 0 (remote disabled)", *calls)
	}
}

func TestMaybeSpawnPricingRefresh_SkipsWhenFresh(t *testing.T) {
	cacheFile := setupRemoteRepo(t, `{"enabled":true,"pricing":{"remote":true}}`)
	// Seed a fresh cache so ShouldRefresh is false.
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	fresh := fmt.Sprintf(`{"fetched_at":%q}`, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(cacheFile, []byte(fresh), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	calls := stubSpawn(t)

	maybeSpawnPricingRefresh(context.Background())

	if *calls != 0 {
		t.Errorf("spawn calls = %d, want 0 (cache is fresh)", *calls)
	}
}
