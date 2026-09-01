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
