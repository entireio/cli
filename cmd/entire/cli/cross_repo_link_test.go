package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestCrossRepoSkipReason(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	protected := []string{home, filepath.Join(home, "go", "pkg", "mod"), filepath.Join(home, ".config", "entire")}
	session := filepath.Join(home, "c")
	for name, tc := range map[string]struct {
		root string
		want bool
	}{
		"sibling repository":       {filepath.Join(home, "d"), false},
		"outside home":             {t.TempDir(), true},
		"submodule of the session": {filepath.Join(session, "vendor", "lib"), true},
		"module cache":             {filepath.Join(home, "go", "pkg", "mod", "x"), true},
		"entire's own directory":   {filepath.Join(home, ".config", "entire", "plugins", "p"), true},
	} {
		if got := crossRepoSkipReason(tc.root, session, protected) != ""; got != tc.want {
			t.Errorf("%s: skipped = %v, want %v", name, got, tc.want)
		}
	}
	if crossRepoSkipReason(filepath.Join(home, "d"), session, nil) == "" {
		t.Error("no home directory: every repository is outside it")
	}
}

func TestAbsoluteToolUsePaths(t *testing.T) {
	t.Parallel()
	got := absoluteToolUsePaths("/work/c", []string{"a.go", "/other/d/b.go", ""}, []string{"new.go"})
	want := []string{"/work/c/a.go", "/other/d/b.go", "/work/c/new.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if filepath.ToSlash(got[i]) != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if absoluteToolUsePaths("", []string{"rel.go"}) != nil {
		t.Error("a relative path with no cwd has no repository")
	}
}

func TestEditsByOtherRepo(t *testing.T) {
	t.Parallel()
	c, d := t.TempDir(), t.TempDir()
	testutil.InitRepo(t, c)
	testutil.InitRepo(t, d)
	ctx := context.Background()
	cCommon := filepath.Join(c, ".git")
	got := editsByOtherRepo(ctx, cCommon, []string{
		filepath.Join(c, "own.go"),
		filepath.Join(d, "pkg", "gone", "x.go"),
		filepath.Join(d, ".git", "config"),
		"relative.go",
	})
	if len(got) != 1 {
		t.Fatalf("got %v, want only d", got)
	}
	for root, rels := range got {
		if !sameResolvedDir(root, d) || len(rels) != 1 || rels[0] != "pkg/gone/x.go" {
			t.Fatalf("got %s: %v", root, rels)
		}
	}
}
