package cli

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

func TestActivityByOtherRepo(t *testing.T) {
	t.Parallel()
	c, d, e := t.TempDir(), t.TempDir(), t.TempDir()
	testutil.InitRepo(t, c)
	testutil.InitRepo(t, d)
	testutil.InitRepo(t, e)
	ctx := context.Background()
	cCommon := filepath.Join(c, ".git")
	got := activityByOtherRepo(ctx, cCommon, crossRepoActivity{edited: []string{
		filepath.Join(c, "own.go"),
		filepath.Join(d, "pkg", "gone", "x.go"),
		filepath.Join(d, ".git", "config"),
		"relative.go",
	}, dirs: []string{c, filepath.Join(d, "pkg"), filepath.Join(e, "new.txt")}})
	if len(got) != 2 {
		t.Fatalf("got %v, want d and e", got)
	}
	for root, rels := range got {
		if sameResolvedDir(root, e) {
			if len(rels) != 0 {
				t.Errorf("e has edits %v; only a shell command worked there", rels)
			}
			delete(got, root)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want only d", got)
	}
	for root, rels := range got {
		if !sameResolvedDir(root, d) || len(rels) != 1 || rels[0] != "pkg/gone/x.go" {
			t.Fatalf("got %s: %v", root, rels)
		}
	}
}

func TestLinkRepos_BackgroundsWhatTheBudgetLeaves(t *testing.T) {
	t.Parallel()
	targets := []linkTarget{{root: "/a"}, {root: "/b"}, {root: "/c"}}
	var ran, spawned []string
	var timeouts []time.Duration
	run := func(_ context.Context, root string, _ []string, timeout time.Duration) bool {
		ran = append(ran, root)
		timeouts = append(timeouts, timeout)
		return root == "/b" // cut short
	}
	spawn := func(root string, _ []string) { spawned = append(spawned, root) }

	linkRepos(context.Background(), targets, time.Now().Add(time.Hour), run, spawn)
	if !slices.Equal(ran, []string{"/a", "/b", "/c"}) || !slices.Equal(spawned, []string{"/b"}) {
		t.Fatalf("ran %v, spawned %v; want all run and the cut-short one backgrounded", ran, spawned)
	}
	for _, timeout := range timeouts {
		if timeout > crossRepoLinkTimeout {
			t.Errorf("timeout %v exceeds the per-repository bound", timeout)
		}
	}

	ran, spawned = nil, nil
	linkRepos(context.Background(), targets, time.Now().Add(-time.Second), run, spawn)
	if len(ran) != 0 || !slices.Equal(spawned, []string{"/a", "/b", "/c"}) {
		t.Fatalf("budget spent: ran %v, spawned %v; want all backgrounded", ran, spawned)
	}
}

func TestLinkSessionArgs_Argv(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
	got := linkSessionArgs{sessionID: "s", from: "/c", since: since, edited: []string{"a.go"}, commits: []string{"abc1234"}, sweep: true}.argv()
	want := []string{"hooks", "git", "link-session", "--session", "s", "--from", "/c", "--since", "2026-11-26T01:02:03Z", "--path", "a.go", "--commit", "abc1234"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := (linkSessionArgs{sessionID: "s", from: "/c", commits: []string{"abc1234"}}).argv(); !slices.Contains(got, "--no-sweep") || slices.Contains(got, "--commit") {
		t.Fatalf("install-only: got %v", got)
	}
}
