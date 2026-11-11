package worktreedir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestIgnoresNormalization checks IgnoresNormalization against what the
// temporary directory's filesystem does: a file created composed (café.txt)
// opens decomposed only where the filesystem ignores normalization. It must
// never claim that for a filesystem that keeps the forms apart, and on macOS,
// whose runners keep temporary files on APFS, it must find it.
func TestIgnoresNormalization(t *testing.T) {
	t.Parallel()
	dir := probeTempDir(t)
	testutil.WriteFile(t, dir, "café.txt", "composed\n")
	root, err := OpenAt(dir)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	_, err = root.Lstat("café.txt")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat: %v", err)
	}
	opens := err == nil
	got := IgnoresNormalization(root, "café.txt")
	if got && !opens {
		t.Errorf("IgnoresNormalization = true, but the decomposed name opens nothing")
	}
	if runtime.GOOS == "darwin" && got != opens {
		t.Errorf("IgnoresNormalization = %v, but the decomposed name opens the composed file: %v", got, opens)
	}
	if gone := IgnoresNormalization(root, "gone/café.txt"); gone != got {
		t.Errorf("IgnoresNormalization(gone/...) = %v, want %v as for its nearest directory that exists", gone, got)
	}
}

// TestNearestDir: the directory asked about a name is its own, or the
// nearest parent that exists when it is gone. A parent that is a file, or a
// link out of the worktree, cannot tell.
func TestNearestDir(t *testing.T) {
	t.Parallel()
	dir := probeTempDir(t)
	testutil.WriteFile(t, dir, "a/b/kept.txt", "kept\n")
	testutil.WriteFile(t, dir, "a/file", "a file\n")
	// Windows has no filesystem IgnoresNormalization answers true for, so
	// what nearestDir makes of a link there does not matter, and Wine, where
	// this runs outside CI, does not follow one as Windows does.
	links := runtime.GOOS != "windows"
	if links && os.Symlink(t.TempDir(), filepath.Join(dir, "out")) != nil {
		links = false
	}
	root, err := OpenAt(dir)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		ok         bool
	}{
		{"a/b/gone.txt", "a/b", true},
		{"a/b/gone/deeper/x.txt", "a/b", true},
		{"gone/x.txt", ".", true},
		{"x.txt", ".", true},
		{"a/file/x.txt", "", false},
		{"out/x.txt", "", false},
	} {
		if tc.name == "out/x.txt" && !links {
			continue
		}
		if got, ok := nearestDir(root, tc.name); got != tc.want || ok != tc.ok {
			t.Errorf("nearestDir(%q) = %q, %v, want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
