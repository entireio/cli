package testdirs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDir_StablePerSurfaceAndNeverRealConfigOrCache(t *testing.T) {
	t.Parallel()

	cfg1, ok := Dir("config")
	if !ok {
		t.Fatal("Dir(config) not available under go test")
	}
	cfg2, ok := Dir("config")
	if !ok || cfg2 != cfg1 {
		t.Fatalf("Dir(config) not stable: first %q, second %q (ok=%v)", cfg1, cfg2, ok)
	}

	cache, ok := Dir("cache")
	if !ok {
		t.Fatal("Dir(cache) not available under go test")
	}
	if cache == cfg1 {
		t.Fatalf("surfaces must not share a directory: %q", cache)
	}

	// The invariant is "never the real app locations" — not "never under
	// $HOME" or even "never under ~/.cache": os.MkdirTemp respects TMPDIR,
	// which is commonly set to places like $HOME/.cache/tmp.
	if home, err := os.UserHomeDir(); err == nil {
		for _, realDir := range []string{
			filepath.Join(home, ".config", "entire"),
			filepath.Join(home, ".cache", "entire"),
		} {
			for _, d := range []string{cfg1, cache} {
				if d == realDir || strings.HasPrefix(d, realDir+string(os.PathSeparator)) {
					t.Fatalf("fallback dir %q resolves under the real %q", d, realDir)
				}
			}
		}
	}

	if _, err := os.Stat(cfg1); err != nil {
		t.Fatalf("fallback dir %q was not created: %v", cfg1, err)
	}
}
