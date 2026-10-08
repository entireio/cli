package userdirs_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// goosWindows is runtime.GOOS on Windows, where unix permission bits are
// synthetic and the tightening step is skipped.
const goosWindows = "windows"

func TestConfig_HonorsEnv(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", "/tmp/explicit/path")
	if got := userdirs.Config(); got != "/tmp/explicit/path" {
		t.Errorf("Config = %q, want /tmp/explicit/path", got)
	}
}

func TestCache_HonorsEnv(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/explicit/cache")
	want := filepath.Join("/tmp/explicit/cache", "entire")
	if got := userdirs.Cache(); got != want {
		t.Errorf("Cache = %q, want %q", got, want)
	}
}

func TestTestRunsNeverResolveRealDirs(t *testing.T) {
	// With no explicit override, a `go test` process must fall back to a
	// throwaway directory — never the real ~/.config/entire or
	// ~/.cache/entire, where it could read or pollute the developer's real
	// state. (The fallback lives under os.TempDir, which may itself be under
	// $HOME via TMPDIR — that's fine; only the real app dirs are
	// off-limits.)
	t.Setenv("ENTIRE_CONFIG_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	for name, tc := range map[string]struct{ got, realDir string }{
		"config": {userdirs.Config(), filepath.Join(home, ".config", "entire")},
		"cache":  {userdirs.Cache(), filepath.Join(home, ".cache", "entire")},
	} {
		if tc.got == "" {
			t.Fatalf("%s: resolved to empty string", name)
		}
		if tc.got == tc.realDir || strings.HasPrefix(tc.got, tc.realDir+string(os.PathSeparator)) {
			t.Fatalf("%s: %q resolves to the real dir %q during tests", name, tc.got, tc.realDir)
		}
	}
}

func TestEnsurePrivateDir_CreatesAt0700(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "entire")
	if err := userdirs.EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != goosWindows && st.Mode().Perm() != 0o700 {
		t.Errorf("mode = %04o, want 0700", st.Mode().Perm())
	}
}

func TestEnsurePrivateDir_TightensLooseExistingDir(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission bits are synthetic on Windows")
	}
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "entire")
	// Reproduces the state left by a version check that ran before login.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := userdirs.EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("mode = %04o, still accessible by group/other", got)
	}
}

func TestEnsurePrivateDir_LeavesAlreadyPrivateDirAlone(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission bits are synthetic on Windows")
	}
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "entire")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := userdirs.EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o500 {
		t.Errorf("mode = %04o, want 0500 preserved", got)
	}
}

func TestEnsurePrivateDir_PreservesOwnerBitsWhileTightening(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("unix permission bits are synthetic on Windows")
	}
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "entire")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := userdirs.EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o500 {
		t.Errorf("mode = %04o, want 0500 (group/other cleared, owner untouched)", got)
	}
}

// A relative override resolves against the working directory, so the same
// environment names a different directory in every process. For the config
// directory that directory holds bearer tokens, and it is usually inside
// whatever repository the command was run from.
func TestRequireAbsoluteOverride(t *testing.T) {
	t.Parallel()

	abs := "/tmp/absolute"
	if runtime.GOOS == goosWindows {
		abs = `C:\tmp\absolute`
	}
	if err := userdirs.RequireAbsoluteOverride("ENTIRE_CONFIG_DIR", abs); err != nil {
		t.Errorf("RequireAbsoluteOverride(%q) = %v, want nil", abs, err)
	}

	for _, value := range []string{"relative/dir", ".", "", "./cfg"} {
		err := userdirs.RequireAbsoluteOverride("ENTIRE_CONFIG_DIR", value)
		if err == nil {
			t.Errorf("RequireAbsoluteOverride(%q) = nil, want an error", value)
			continue
		}
		// The name has to be the one the user can set, or the message tells
		// them nothing actionable.
		if !strings.Contains(err.Error(), "ENTIRE_CONFIG_DIR") {
			t.Errorf("RequireAbsoluteOverride(%q) error = %q, want it to name the variable", value, err)
		}
	}
}

// The string forms cannot report, so the roots are where the refusal has to
// land — and it must land before the directory is created, since creating one
// under a cwd-relative path is the mistake being reported.
func TestConfigRoot_RefusesRelativeOverride(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", "relative-config")

	if _, err := userdirs.ConfigRoot(); err == nil {
		t.Fatal("ConfigRoot() = nil error, want a rejected override")
	}
	if _, err := os.Stat("relative-config"); err == nil {
		t.Error("ConfigRoot() created the directory it was refusing")
	}
}

func TestCacheRoot_RefusesRelativeOverride(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "relative-cache")

	if _, err := userdirs.CacheRoot(); err == nil {
		t.Fatal("CacheRoot() = nil error, want a rejected override")
	}
	if _, err := os.Stat("relative-cache"); err == nil {
		t.Error("CacheRoot() created the directory it was refusing")
	}
}
