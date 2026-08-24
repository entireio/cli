package settings

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// goosWindows avoids repeating the GOOS literal (file permissions are not
// meaningful on Windows, so the mode assertion is skipped there).
const goosWindows = "windows"

func TestSaveUserSettings_CreatesDirAndRoundTrips(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "nested", "entire")
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)

	in := &UserSettings{Global: &GlobalConfig{
		Enabled:        true,
		ExcludePaths:   []string{"~/oss/**"},
		ExcludeOrigins: []string{"github.com/acme/*"},
	}}
	if err := SaveUserSettings(t.Context(), in); err != nil {
		t.Fatalf("SaveUserSettings: %v", err)
	}

	out, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatalf("LoadUserSettings after save: %v", err)
	}
	if out.Global == nil || !out.Global.Enabled {
		t.Fatalf("round trip lost global config: %+v", out.Global)
	}
	if len(out.Global.ExcludePaths) != 1 || len(out.Global.ExcludeOrigins) != 1 {
		t.Fatalf("round trip lost exclude lists: %+v", out.Global)
	}

	if runtime.GOOS != goosWindows {
		info, err := os.Stat(filepath.Join(cfg, UserSettingsFileName))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("user settings file mode = %o, want 600", perm)
		}
	}
}

// TestModifyUserSettings_ConcurrentWritersLoseNoUpdates pins the file-lock
// contract: two concurrent writers — one growing the exclude list, one
// flipping enabled — must both land. Dropping the flock.Acquire in
// ModifyUserSettings turns the read-modify-write into a lost-update race this
// test catches.
func TestModifyUserSettings_ConcurrentWritersLoseNoUpdates(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	const iterations = 25
	errs := make(chan error, 2)
	go func() {
		for i := range iterations {
			if err := ModifyUserSettings(ctx, func(us *UserSettings) error {
				if us.Global == nil {
					us.Global = &GlobalConfig{}
				}
				us.Global.ExcludePaths = append(us.Global.ExcludePaths, "/srv/a/"+string(rune('a'+i))+"/**")
				return nil
			}); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	go func() {
		for i := range iterations {
			enabled := i%2 == 0
			if err := ModifyUserSettings(ctx, func(us *UserSettings) error {
				if us.Global == nil {
					us.Global = &GlobalConfig{}
				}
				us.Global.Enabled = enabled
				return nil
			}); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ModifyUserSettings: %v", err)
		}
	}

	out, err := LoadUserSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Global == nil || len(out.Global.ExcludePaths) != iterations {
		got := 0
		if out.Global != nil {
			got = len(out.Global.ExcludePaths)
		}
		t.Fatalf("lost update: %d exclude paths survived, want %d", got, iterations)
	}
	// The flipper's last write (i = iterations-1, odd → false) must survive too.
	if out.Global.Enabled != ((iterations-1)%2 == 0) {
		t.Fatalf("lost update: enabled = %v after final flip", out.Global.Enabled)
	}
}

// TestRepoSettingsWrites_ClearInvisibleRoutingCache pins the invalidation
// contract documented on paths' invisibleCache: creating a repo settings
// file — through the struct save path (saveToFile) or the raw path (saveRaw)
// — is a discriminator write, so the writer process must observe runtime-data
// routing flip from the git common dir back to the worktree.
func TestRepoSettingsWrites_ClearInvisibleRoutingCache(t *testing.T) {
	cases := []struct {
		name  string
		write func(ctx context.Context) error
	}{
		{"struct save (saveToFile)", func(ctx context.Context) error {
			return Save(ctx, &EntireSettings{Enabled: true})
		}},
		{"raw save (saveRaw)", func(ctx context.Context) error {
			path, raw, _, err := LoadLocalRaw(ctx)
			if err != nil {
				return err
			}
			raw["enabled"] = json.RawMessage("false")
			return SaveLocalRaw(path, raw)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if resolved, err := filepath.EvalSymlinks(dir); err == nil {
				dir = resolved
			}
			testutil.InitRepo(t, dir)
			t.Chdir(dir)
			cfg := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", cfg)
			if err := os.WriteFile(filepath.Join(cfg, UserSettingsFileName), []byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			reset := func() {
				paths.ClearWorktreeRootCache()
				paths.ClearInvisibleRuntimeCache()
				ClearGlobalModeCache()
			}
			reset()
			t.Cleanup(reset)

			routed, err := paths.AbsPath(t.Context(), paths.EntireMetadataDir)
			if err != nil {
				t.Fatal(err)
			}
			worktreeMeta := filepath.Join(dir, ".entire", "metadata")
			if routed == worktreeMeta {
				t.Fatalf("precondition: a globally tracked repo must route runtime data into .git, got %s", routed)
			}
			if err := tc.write(t.Context()); err != nil {
				t.Fatalf("settings write: %v", err)
			}
			after, err := paths.AbsPath(t.Context(), paths.EntireMetadataDir)
			if err != nil {
				t.Fatal(err)
			}
			if after != worktreeMeta {
				t.Fatalf("settings write must flip routing worktree-ward in-process: got %s, want %s", after, worktreeMeta)
			}
		})
	}
}

// TestSaveUserSettings_PreservesSymlinkedSettingsFile pins the symlink
// contract: dotfile managers commonly symlink ~/.config/entire/settings.json,
// and a rename-over-the-link atomic write would replace the link with a
// regular file, silently detaching the managed target. The save must follow
// the link and rewrite its target — mirroring LoadUserSettings' documented
// symlink-following read path.
func TestSaveUserSettings_PreservesSymlinkedSettingsFile(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("symlink creation needs privileges on Windows")
	}
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Cleanup(ClearGlobalModeCache)

	target := filepath.Join(t.TempDir(), "managed-settings.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg, UserSettingsFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := SaveUserSettings(t.Context(), &UserSettings{Global: &GlobalConfig{Enabled: true}}); err != nil {
		t.Fatalf("SaveUserSettings: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("settings.json symlink was replaced by a regular file (mode %v)", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("symlink target unreadable after save: %v", err)
	}
	if !strings.Contains(string(data), `"enabled": true`) {
		t.Fatalf("symlink target not updated, got: %s", data)
	}
}

// TestSaveUserSettings_LoadModifySavePreservesExcludes pins the writer
// contract used by `enable --global`: flipping enabled must not drop the
// exclude lists already in the file.
func TestSaveUserSettings_LoadModifySavePreservesExcludes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	writeUserSettings(t, dir, `{"global":{"enabled":false,"exclude_paths":["~/oss/**"],"exclude_origins":["github.com/acme/*"]}}`)

	us, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	us.Global.Enabled = true
	if err := SaveUserSettings(t.Context(), us); err != nil {
		t.Fatal(err)
	}

	out, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Global.Enabled || len(out.Global.ExcludePaths) != 1 || len(out.Global.ExcludeOrigins) != 1 {
		t.Fatalf("enable flip lost data: %+v", out.Global)
	}
}

func TestGlobalModeActive_MemoizedPerProcess(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	ClearGlobalModeCache()
	t.Cleanup(ClearGlobalModeCache)

	if !GlobalModeActive(t.Context()) {
		t.Fatal("global mode should be active")
	}

	// Flip the file behind the cache: the memoized answer must hold — hooks
	// are one-shot processes and every gate must see one consistent answer.
	writeUserSettings(t, cfg, `{"global":{"enabled":false}}`)
	if !GlobalModeActive(t.Context()) {
		t.Fatal("memoized result must not change mid-process")
	}

	ClearGlobalModeCache()
	if GlobalModeActive(t.Context()) {
		t.Fatal("after cache reset the new file content must win")
	}
}

// TestSaveUserSettings_InvalidatesGlobalModeCache pins that a writer process
// observes its own write without an explicit cache reset.
func TestSaveUserSettings_InvalidatesGlobalModeCache(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	ClearGlobalModeCache()
	t.Cleanup(ClearGlobalModeCache)

	if !GlobalModeActive(t.Context()) {
		t.Fatal("global mode should be active")
	}
	if err := SaveUserSettings(t.Context(), &UserSettings{Global: &GlobalConfig{Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if GlobalModeActive(t.Context()) {
		t.Fatal("SaveUserSettings must invalidate the memoized gate")
	}
}
