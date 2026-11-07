package worktreedir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newCaseProbeTree builds, on a case-sensitive filesystem, a directory whose
// lookups respect case (sensitive/, where a.TXT is another file than A.txt)
// and one that stands in for a directory whose lookups ignore case
// (folding/, where a.TXT is a hard link to A.txt).
func newCaseProbeTree(t *testing.T) *CaseProbe {
	t.Helper()
	dir := t.TempDir()
	testutil.WriteFile(t, dir, "sensitive/A.txt", "a\n")
	if _, err := os.Lstat(filepath.Join(dir, "sensitive", "a.TXT")); err == nil {
		t.Skip("needs a case-sensitive filesystem; TestCaseProbeWindows covers the other side")
	}
	testutil.WriteFile(t, dir, "sensitive/a.TXT", "another file\n")
	testutil.WriteFile(t, dir, "folding/A.txt", "a\n")
	if err := os.Link(filepath.Join(dir, "folding", "A.txt"), filepath.Join(dir, "folding", "a.TXT")); err != nil {
		t.Fatalf("link: %v", err)
	}
	testutil.WriteFile(t, dir, "folding/digits/123", "no letters\n")
	return NewCaseProbe(dir)
}

func TestCaseProbe_IgnoresCase(t *testing.T) {
	t.Parallel()
	probe := newCaseProbeTree(t)
	for _, tc := range []struct {
		dir  string
		want bool
		why  string
	}{
		{".", false, "FOLDING opens nothing"},
		{"sensitive", false, "a.TXT is another file"},
		{"folding", true, "a.TXT opens A.txt"},
		{"folding/digits", true, "no entry with a letter: the parent answers"},
		{"folding/gone", true, "a directory that is gone: the parent answers"},
	} {
		if got := probe.IgnoresCase(tc.dir); got != tc.want {
			t.Errorf("IgnoresCase(%q) = %v, want %v: %s", tc.dir, got, tc.want, tc.why)
		}
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
		{"folding/gone/README.md", "folding/gone/readme.md", true, "a gone directory is answered by folding/"},
		{"sensitive/README.md", "sensitive/readme.md", false, "sensitive/ respects case"},
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
	for _, d := range []string{".", "sub", "sub/gone"} {
		if got := probe.IgnoresCase(d); got != want {
			t.Errorf("IgnoresCase(%q) = %v, want %v as the filesystem does", d, got, want)
		}
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
