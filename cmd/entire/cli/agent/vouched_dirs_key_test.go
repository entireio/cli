package agent

import (
	"path/filepath"
	"runtime"
	"testing"
)

// The policy key must survive the two producers spelling one directory
// differently. Readers pass paths.WorktreeRoot, which is git's
// `rev-parse --show-toplevel` verbatim; settings derives its root from
// entiredir.PathTo, whose filepath.Join re-Cleans the same path. On Windows
// those differ (C:/repo vs C:\repo) and the plain string compare this replaced
// silently disabled the whole feature there.
func TestKeyFor_EquatesSpellingsOfOneDirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	want := keyFor(base)

	// filepath.Join is what the settings side puts the path through.
	viaJoin := filepath.Dir(filepath.Dir(filepath.Join(base, ".entire", "settings.json")))
	if got := keyFor(viaJoin); got != want {
		t.Errorf("keyFor(via filepath.Join) = %q, want %q; the two producers must agree", got, want)
	}

	for _, spelling := range []string{
		base + string(filepath.Separator),
		filepath.Join(base, "."),
		filepath.Join(base, "sub", ".."),
	} {
		if got := keyFor(spelling); got != want {
			t.Errorf("keyFor(%q) = %q, want %q", spelling, got, want)
		}
	}
}

// Distinct directories must still key distinctly, or the scope is not a scope.
func TestKeyFor_DistinguishesDifferentDirectories(t *testing.T) {
	t.Parallel()

	first, second := t.TempDir(), t.TempDir()
	if keyFor(first) == keyFor(second) {
		t.Errorf("two different worktrees (%q, %q) produced the same key", first, second)
	}
	if keyFor("") != "" {
		t.Error(`keyFor("") must stay empty so an unset policy cannot match a real root`)
	}
}

// Case folding is Windows-only: there two spellings differing in case are one
// directory, while on Unix they are two.
func TestKeyFor_CaseHandlingMatchesThePlatform(t *testing.T) {
	t.Parallel()

	lower, upper := keyFor("/tmp/repo"), keyFor("/tmp/REPO")
	if runtime.GOOS == goosWindows {
		if lower != upper {
			t.Errorf("on Windows /tmp/repo and /tmp/REPO are one directory; got %q vs %q", lower, upper)
		}
		return
	}
	if lower == upper {
		t.Errorf("on %s these are different directories; got %q for both", runtime.GOOS, lower)
	}
}
