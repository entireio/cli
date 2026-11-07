package worktreedir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newCaseProbeTree builds, on a case-sensitive filesystem, directories whose
// lookups respect case: the root, sensitive/ (where a.TXT opens nothing) and
// linked/ (where a.TXT is a hard link to A.txt, an entry of its own). Its
// probe stands in for a filesystem whose other directories ignore case:
// folding/, folding/digits/ (only a name without letters) and big/ (more
// entries than the probe reads).
func newCaseProbeTree(t *testing.T) *CaseProbe {
	t.Helper()
	dir := t.TempDir()
	testutil.WriteFile(t, dir, "sensitive/A.txt", "a\n")
	if _, err := os.Lstat(filepath.Join(dir, "sensitive", "a.TXT")); err == nil {
		t.Skip("needs a case-sensitive filesystem; TestCaseProbeWindows covers the other side")
	}
	testutil.WriteFile(t, dir, "linked/A.txt", "a\n")
	if err := os.Link(filepath.Join(dir, "linked", "A.txt"), filepath.Join(dir, "linked", "a.TXT")); err != nil {
		t.Fatalf("link: %v", err)
	}
	testutil.WriteFile(t, dir, "folding/A.txt", "a\n")
	testutil.WriteFile(t, dir, "folding/digits/123", "no letters\n")
	for _, name := range []string{"A.txt", "b", "c", "d", "e"} {
		testutil.WriteFile(t, dir, "big/"+name, "one of five\n")
	}
	probe := NewCaseProbe(dir)
	probe.maxEntries = 4
	probe.lookUp = foldingLookUp
	return probe
}

// foldingLookUp is a lookup that ignores case in every directory but the
// root and sensitive/: a name that opens nothing opens the entry it names in
// another case.
func foldingLookUp(d *os.Root, dir, name string) (fs.FileInfo, error) {
	info, err := d.Lstat(name)
	if !errors.Is(err, fs.ErrNotExist) || dir == "." || dir == "sensitive" {
		return info, err
	}
	entries, readErr := osroot.ReadDir(d, ".")
	if readErr != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), name) {
			return d.Lstat(entry.Name())
		}
	}
	return nil, err
}

func TestCaseProbe_IgnoresCase(t *testing.T) {
	t.Parallel()
	probe := newCaseProbeTree(t)
	for _, tc := range []struct {
		dir  string
		want bool
		why  string
	}{
		{".", false, "BIG opens nothing"},
		{"sensitive", false, "a.TXT opens nothing"},
		{"linked", false, "a.TXT opens A.txt, but as an entry of its own"},
		{"folding", true, "a.TXT opens A.txt"},
		{"folding/digits", false, "no entry with a letter cannot tell, whatever folding/ does"},
		{"folding/gone", false, "a directory that is gone cannot tell"},
		{"big", false, "more entries than the probe reads"},
	} {
		if got := probe.IgnoresCase(tc.dir); got != tc.want {
			t.Errorf("IgnoresCase(%q) = %v, want %v: %s", tc.dir, got, tc.want, tc.why)
		}
	}

	probe = newCaseProbeTree(t)
	probe.maxEntries = 5
	if !probe.IgnoresCase("big") {
		t.Error("IgnoresCase(big) = false with all five entries read, want true")
	}
}

func TestCaseProbe_SameEntry(t *testing.T) {
	t.Parallel()
	probe := newCaseProbeTree(t)
	for _, tc := range []struct {
		file, spelling string
		want           bool
		why            string
	}{
		{"folding/README.md", "folding/readme.md", true, "folding/ ignores case, even for a file that is gone"},
		{"folding/Gone/README.md", "folding/gone/README.md", true, "a gone directory's own name is asked of folding/"},
		{"folding/gone/README.md", "folding/gone/readme.md", false, "a gone directory cannot tell"},
		{"sensitive/README.md", "sensitive/readme.md", false, "sensitive/ respects case"},
		{"linked/README.md", "linked/readme.md", false, "linked/ respects case"},
		{"folding/README.md", "Folding/README.md", false, "the root respects case"},
		{"folding/README.md", "folding/README.md", true, "the same spelling"},
		{"folding/README.md", "folding/READ.md", false, "not the same name in another case"},
		{"folding/README.md", "folding/x/README.md", false, "another depth"},
	} {
		if got := probe.SameEntry(tc.file, tc.spelling); got != tc.want {
			t.Errorf("SameEntry(%q, %q) = %v, want %v: %s", tc.file, tc.spelling, got, tc.want, tc.why)
		}
	}
}

// TestCaseProbeWindows checks the probe against the filesystem itself. CI's
// Windows job runs it by its name, on a filesystem that ignores case.
func TestCaseProbeWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.WriteFile(t, dir, "sub/Probe.txt", "probe\n")
	info, err := os.Lstat(filepath.Join(dir, "sub", "Probe.txt"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	folded, err := os.Lstat(filepath.Join(dir, "sub", "pROBE.TXT"))
	want := err == nil && os.SameFile(info, folded)
	probe := NewCaseProbe(dir)
	for _, d := range []string{".", "sub"} {
		if got := probe.IgnoresCase(d); got != want {
			t.Errorf("IgnoresCase(%q) = %v, want %v as the filesystem does", d, got, want)
		}
	}
	if probe.IgnoresCase("sub/gone") {
		t.Error("IgnoresCase(sub/gone) = true for a directory that is gone, want false")
	}
	if got := probe.SameEntry("Sub/GONE.txt", "sub/gone.txt"); got != want {
		t.Errorf("SameEntry = %v, want %v as the filesystem does", got, want)
	}
}

func TestCaseProbe_UnopenableWorktreeAnswersFalse(t *testing.T) {
	t.Parallel()
	probe := NewCaseProbe(filepath.Join(t.TempDir(), "missing"))
	if probe.IgnoresCase(".") || probe.SameEntry("A", "a") {
		t.Error("a probe without a worktree must answer false")
	}
}

func TestSwapASCIICase(t *testing.T) {
	t.Parallel()
	if got := SwapASCIICase("README.md-1é"); got != "readme.MD-1é" {
		t.Errorf("SwapASCIICase = %q", got)
	}
}
