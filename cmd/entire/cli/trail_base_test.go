package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestParseSymrefHEAD(t *testing.T) {
	t.Parallel()
	tests := []struct {
		out  string
		want string
		ok   bool
	}{
		{"ref: refs/heads/release\tHEAD\n3f1c0a\tHEAD\n", "release", true},
		{"ref: refs/heads/feature/x\tHEAD\n", "feature/x", true},
		{"3f1c0a\tHEAD\n", "", false},
		{"ref: refs/tags/v1\tHEAD\n", "", false},
		{"ref: refs/heads/main\trefs/heads/other\n", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := parseSymrefHEAD(tt.out)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseSymrefHEAD(%q) = (%q, %v), want (%q, %v)", tt.out, got, ok, tt.want, tt.ok)
		}
	}
}

// initDefaultBranchRemote makes a bare remote whose HEAD names release, and a
// clone of it without a local origin/HEAD (as when origin is added by hand).
func initDefaultBranchRemote(t *testing.T) (dir string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	testutil.RunGit(t, root, "init", "--bare", bare)
	dir = filepath.Join(root, "work")
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "branch", "-M", "main")
	testutil.RunGit(t, dir, "remote", "add", "origin", bare)
	testutil.RunGit(t, dir, "push", "origin", "main", "main:release")
	testutil.RunGit(t, root, "--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/release")
	testutil.RunGit(t, dir, "fetch", "origin")
	// Recent git creates origin/HEAD on fetch (remote.<name>.followRemoteHEAD);
	// drop it to model a clone whose origin was added or re-pointed by hand.
	testutil.RunGit(t, dir, "remote", "set-head", "origin", "-d")
	return dir
}

// Not parallel: chdirs into the checkout, as trail create runs from it.
func TestRemoteDefaultBranch(t *testing.T) {
	dir := initDefaultBranchRemote(t)
	t.Chdir(dir)
	if got, ok := remoteDefaultBranch(context.Background(), "origin"); !ok || got != "release" {
		t.Fatalf("remoteDefaultBranch(origin) = (%q, %v), want (release, true)", got, ok)
	}
	missing := filepath.Join(t.TempDir(), "missing.git")
	if got, ok := remoteDefaultBranch(context.Background(), missing); ok || got != "" {
		t.Fatalf("remoteDefaultBranch(missing) = (%q, %v), want unknown", got, ok)
	}
}

// Not parallel: swaps the package-level remote lookup seam.
func TestResolveTrailCreateBase(t *testing.T) {
	dir := initDefaultBranchRemote(t)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	stubRemote := func(t *testing.T, name string, ok bool) {
		t.Helper()
		prev := trailRemoteDefaultBranch
		trailRemoteDefaultBranch = func(context.Context, string) (string, bool) { return name, ok }
		t.Cleanup(func() { trailRemoteDefaultBranch = prev })
	}
	t.Run("explicit base wins", func(t *testing.T) {
		stubRemote(t, "release", true)
		send, naming := resolveTrailCreateBase(t.Context(), repo, " develop ")
		if send != "develop" || naming != "develop" {
			t.Fatalf("got (%q, %q)", send, naming)
		}
	})
	t.Run("remote default when origin/HEAD is missing", func(t *testing.T) {
		stubRemote(t, "release", true)
		send, naming := resolveTrailCreateBase(t.Context(), repo, "")
		if send != "release" || naming != "release" {
			t.Fatalf("got (%q, %q), want release for both", send, naming)
		}
	})
	t.Run("unknown sends nothing but keeps the naming guess", func(t *testing.T) {
		stubRemote(t, "", false)
		send, naming := resolveTrailCreateBase(t.Context(), repo, "")
		if send != "" || naming != "main" {
			t.Fatalf("got (%q, %q), want (\"\", main): never send a guess", send, naming)
		}
	})
	t.Run("invalid remote answer is ignored", func(t *testing.T) {
		stubRemote(t, "bad..name", true)
		send, _ := resolveTrailCreateBase(t.Context(), repo, "")
		if send != "" {
			t.Fatalf("send = %q, want an invalid name dropped", send)
		}
	})
	t.Run("origin/HEAD outside origin's branches is ignored", func(t *testing.T) {
		stubRemote(t, "", false)
		testutil.RunGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/release")
		t.Cleanup(func() { testutil.RunGit(t, dir, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD") })
		if send, _ := resolveTrailCreateBase(t.Context(), repo, ""); send != "" {
			t.Fatalf("send = %q, want a hand-set origin/HEAD outside refs/remotes/origin ignored", send)
		}
	})
	t.Run("origin's answer beats a stale origin/HEAD", func(t *testing.T) {
		// fetch leaves an existing origin/HEAD alone when the upstream default
		// moves and the old branch survives, so the local ref can be stale.
		stubRemote(t, "release", true)
		testutil.RunGit(t, dir, "remote", "set-head", "origin", "main")
		t.Cleanup(func() { testutil.RunGit(t, dir, "remote", "set-head", "origin", "-d") })
		send, naming := resolveTrailCreateBase(t.Context(), repo, "")
		if send != "release" || naming != "release" {
			t.Fatalf("got (%q, %q), want release from origin over a stale origin/HEAD", send, naming)
		}
	})
	t.Run("local origin/HEAD answers when origin cannot", func(t *testing.T) {
		stubRemote(t, "", false)
		testutil.RunGit(t, dir, "remote", "set-head", "origin", "release")
		t.Cleanup(func() { testutil.RunGit(t, dir, "remote", "set-head", "origin", "-d") })
		send, naming := resolveTrailCreateBase(t.Context(), repo, "")
		if send != "release" || naming != "release" {
			t.Fatalf("got (%q, %q), want release from origin/HEAD", send, naming)
		}
	})
}
