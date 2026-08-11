package settings

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
