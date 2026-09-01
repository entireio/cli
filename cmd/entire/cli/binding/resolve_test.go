package binding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	resolved, err := filepath.EvalSymlinks(dir) // macOS /var → /private/var
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestResolveRepoForPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("path inside a repo", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		sub := filepath.Join(repo, "pkg", "deep")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		id, ok := ResolveRepoForPath(ctx, filepath.Join(sub, "file.go"))
		if !ok {
			t.Fatal("expected repo hit")
		}
		if id.WorktreeRoot != repo {
			t.Errorf("root = %q, want %q", id.WorktreeRoot, repo)
		}
		if !filepath.IsAbs(id.CommonDir) {
			t.Errorf("common dir must be absolute, got %q", id.CommonDir)
		}
	})

	t.Run("nonexistent file, existing parent", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		id, ok := ResolveRepoForPath(ctx, filepath.Join(repo, "not", "yet", "created.go"))
		if !ok || id.WorktreeRoot != repo {
			t.Fatalf("ancestor walk failed: ok=%v id=%+v", ok, id)
		}
	})

	t.Run("path outside any repo", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if _, ok := ResolveRepoForPath(ctx, filepath.Join(dir, "f.txt")); ok {
			t.Fatal("non-repo path must miss")
		}
	})

	t.Run("relative path is rejected", func(t *testing.T) {
		t.Parallel()
		if _, ok := ResolveRepoForPath(ctx, "relative/path.go"); ok {
			t.Fatal("relative input must miss (caller's job to absolutize)")
		}
	})

	t.Run("symlinked path and real path yield identical identity", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		link := filepath.Join(t.TempDir(), "repo-link")
		if err := os.Symlink(repo, link); err != nil {
			t.Fatal(err)
		}
		viaLink, ok := ResolveRepoForPath(ctx, filepath.Join(link, "f.go"))
		if !ok {
			t.Fatal("expected hit via symlink")
		}
		viaReal, ok := ResolveRepoForPath(ctx, filepath.Join(repo, "f.go"))
		if !ok {
			t.Fatal("expected hit via real path")
		}
		if viaLink != viaReal {
			t.Errorf("same clone must yield one identity: via link %+v, via real path %+v", viaLink, viaReal)
		}
	})

	t.Run("linked worktree resolves to shared common dir", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		testutil.WriteFile(t, repo, "a.txt", "x")
		testutil.GitAdd(t, repo, "a.txt")
		testutil.GitCommit(t, repo, "init")
		wt := filepath.Join(t.TempDir(), "wt")
		runGit(t, repo, "worktree", "add", wt)
		wtResolved, err := filepath.EvalSymlinks(wt)
		if err != nil {
			t.Fatal(err)
		}
		id, ok := ResolveRepoForPath(ctx, filepath.Join(wtResolved, "a.txt"))
		if !ok {
			t.Fatal("expected hit")
		}
		mainID, _ := ResolveRepoForPath(ctx, filepath.Join(repo, "a.txt"))
		if id.CommonDir != mainID.CommonDir {
			t.Errorf("linked worktree common dir %q != main %q", id.CommonDir, mainID.CommonDir)
		}
		if id.WorktreeRoot == mainID.WorktreeRoot {
			t.Error("worktree roots must differ")
		}
	})
}

// The flagless path is what pre-2.31 gits execute; it must produce output
// byte-identical to the --path-format=absolute path, including through
// symlinks, or old-git machines get split CommonDir keys.
func TestRunRevParse_FlaglessFallbackMatchesFlagPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newRepo(t)
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	viaLink, ok := runRevParse(ctx, link, false)
	if !ok {
		t.Fatal("flagless resolve via symlink failed")
	}
	viaReal, ok := runRevParse(ctx, repo, false)
	if !ok {
		t.Fatal("flagless resolve via real path failed")
	}
	if viaLink != viaReal {
		t.Errorf("flagless identity differs via symlink: %+v vs %+v", viaLink, viaReal)
	}
	viaFlag, ok := runRevParse(ctx, repo, true)
	if !ok {
		t.Fatal("flag resolve failed")
	}
	if viaLink != viaFlag {
		t.Errorf("flagless fallback %+v differs from flag path %+v", viaLink, viaFlag)
	}
}

func TestResolveRepoForPath_CachesPerDirectory(t *testing.T) {
	// Not parallel: inspects global cache state.
	ctx := context.Background()
	ClearResolveCache()
	repo := newRepo(t)
	sub := filepath.Join(repo, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ResolveRepoForPath(ctx, filepath.Join(sub, "a.go"))
	ResolveRepoForPath(ctx, filepath.Join(sub, "b.go"))
	if n := ResolveCacheSizeForTesting(); n != 1 {
		t.Errorf("same-dir paths must share one cache entry, got %d", n)
	}
	// not-a-repo results are cached too
	out := t.TempDir()
	ResolveRepoForPath(ctx, filepath.Join(out, "x"))
	ResolveRepoForPath(ctx, filepath.Join(out, "y"))
	if n := ResolveCacheSizeForTesting(); n != 2 {
		t.Errorf("negative results must cache, got %d entries", n)
	}
}
