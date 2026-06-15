package userdirs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

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
