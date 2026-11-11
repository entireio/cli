package worktreedir

import (
	"errors"
	"io/fs"
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
	got := IgnoresNormalization(root)
	if got && !opens {
		t.Errorf("IgnoresNormalization = true, but the decomposed name opens nothing")
	}
	if runtime.GOOS == "darwin" && got != opens {
		t.Errorf("IgnoresNormalization = %v, but the decomposed name opens the composed file: %v", got, opens)
	}
}
