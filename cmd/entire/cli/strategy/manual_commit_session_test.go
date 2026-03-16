package strategy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathForComparison(t *testing.T) {
	t.Parallel()

	t.Run("identical paths", func(t *testing.T) {
		t.Parallel()
		a := resolvePathForComparison("/some/path/to/repo")
		b := resolvePathForComparison("/some/path/to/repo")
		if a != b {
			t.Errorf("identical paths should resolve equally: %q != %q", a, b)
		}
	})

	t.Run("trailing slash normalization", func(t *testing.T) {
		t.Parallel()
		a := resolvePathForComparison("/some/path/to/repo/")
		b := resolvePathForComparison("/some/path/to/repo")
		if a != b {
			t.Errorf("trailing slash mismatch: %q != %q", a, b)
		}
	})

	t.Run("symlink resolution", func(t *testing.T) {
		t.Parallel()
		realDir := t.TempDir()
		symlinkDir := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(realDir, symlinkDir); err != nil {
			t.Skipf("cannot create symlinks: %v", err)
		}

		resolved1 := resolvePathForComparison(realDir)
		resolved2 := resolvePathForComparison(symlinkDir)
		if resolved1 != resolved2 {
			t.Errorf("symlinked paths should resolve equally: %q != %q", resolved1, resolved2)
		}
	})

	t.Run("nonexistent path returns cleaned", func(t *testing.T) {
		t.Parallel()
		p := "/nonexistent/path/to/repo/"
		got := resolvePathForComparison(p)
		want := filepath.Clean(p)
		if got != want {
			t.Errorf("nonexistent path: got %q, want %q", got, want)
		}
	})
}
