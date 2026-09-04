package execx

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests set PATH, which is process-global, so they cannot be parallel.

func join(dirs ...string) string {
	return strings.Join(dirs, string(os.PathListSeparator))
}

func abs(t *testing.T, parts ...string) string {
	t.Helper()
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func TestPathScanDirs_DropsRelativeEntries(t *testing.T) {
	good := abs(t, "usr", "bin")
	t.Setenv("PATH", join("relative/bin", good, "./dot", "..", "sub"))

	got := PathScanDirs()

	if len(got) != 1 || got[0] != good {
		t.Fatalf("PathScanDirs() = %q, want exactly [%q]", got, good)
	}
}

func TestPathScanDirs_DropsEmptyEntries(t *testing.T) {
	good := abs(t, "usr", "bin")
	t.Setenv("PATH", join("", good, ""))

	got := PathScanDirs()

	if len(got) != 1 || got[0] != good {
		t.Fatalf("PathScanDirs() = %q, want exactly [%q]", got, good)
	}
}

func TestPathScanDirs_KeepsAbsoluteEntriesInOrder(t *testing.T) {
	first, second := abs(t, "a"), abs(t, "b")
	t.Setenv("PATH", join(first, second))

	got := PathScanDirs()

	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("PathScanDirs() = %q, want [%q %q]", got, first, second)
	}
}

func TestPathScanDirs_EmptyPATH(t *testing.T) {
	t.Setenv("PATH", "")

	if got := PathScanDirs(); len(got) != 0 {
		t.Fatalf("PathScanDirs() = %q, want empty", got)
	}
}
