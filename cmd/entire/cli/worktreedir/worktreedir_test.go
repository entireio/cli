package worktreedir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestName(t *testing.T) {
	t.Parallel()

	root := filepath.Join(string(filepath.Separator), "repo")

	t.Run("relative name is kept", func(t *testing.T) {
		t.Parallel()
		got, err := Name(root, "api/src/types.ts")
		if err != nil {
			t.Fatalf("Name: %v", err)
		}
		if got != "api/src/types.ts" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("absolute path inside the worktree is relativized", func(t *testing.T) {
		t.Parallel()
		got, err := Name(root, filepath.Join(root, "api", "file.ts"))
		if err != nil {
			t.Fatalf("Name: %v", err)
		}
		if got != "api/file.ts" {
			t.Errorf("got %q", got)
		}
	})

	for _, p := range []string{
		"",
		".",
		"../escape",
		"api/../../escape",
	} {
		t.Run("rejects "+p, func(t *testing.T) {
			t.Parallel()
			if got, err := Name(root, p); err == nil {
				t.Errorf("Name(%q) = %q, want error", p, got)
			}
		})
	}

	// Windows has two forms that are neither absolute nor volume-prefixed yet
	// name nothing inside the worktree: "C:foo" (drive-relative, separator-free)
	// and "\\foo" (rooted-relative, where volumeNameLen is 0 because a single
	// leading backslash is neither a volume nor a UNC prefix). filepath.IsLocal
	// is what keeps both off the name branch. On Unix each is an ordinary
	// relative filename and must be accepted, which is why this asserts each
	// platform's answer rather than skipping.
	for _, p := range []string{"C:foo", `\foo\bar`} {
		t.Run("non-local path "+p, func(t *testing.T) {
			t.Parallel()
			got, err := Name(root, p)
			if !filepath.IsLocal(p) {
				if err == nil {
					t.Errorf("Name(%q) = %q, want error on a volume-aware platform", p, got)
				}
				return
			}
			if err != nil {
				t.Errorf("Name(%q) = %q, %v; want it kept as a plain relative name", p, got, err)
			}
		})
	}

	t.Run("rejects an absolute path outside the worktree", func(t *testing.T) {
		t.Parallel()
		outside := filepath.Join(string(filepath.Separator), "elsewhere", "file.ts")
		if got, err := Name(root, outside); err == nil {
			t.Errorf("Name(%q) = %q, want error", outside, got)
		}
	})
}

// TestNameFollowingLinks covers the property Name alone cannot give: os.Root
// refuses an absolute symlink target even when it resolves inside the root, so
// a user-owned working-tree file pointed at a monorepo's shared config with an
// absolute link needs the link resolved before containment is judged.
func TestNameFollowingLinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(shared, "vercel.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("absolute link inside the worktree resolves to its target", func(t *testing.T) {
		t.Parallel()
		link := filepath.Join(root, "abs.json")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		got, err := NameFollowingLinks(root, "abs.json")
		if err != nil {
			t.Fatalf("NameFollowingLinks() error = %v", err)
		}
		if got != "shared/vercel.json" {
			t.Errorf("NameFollowingLinks() = %q, want shared/vercel.json", got)
		}
	})

	t.Run("relative link inside the worktree resolves too", func(t *testing.T) {
		t.Parallel()
		link := filepath.Join(root, "rel.json")
		if err := os.Symlink(filepath.Join("shared", "vercel.json"), link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		got, err := NameFollowingLinks(root, "rel.json")
		if err != nil {
			t.Fatalf("NameFollowingLinks() error = %v", err)
		}
		if got != "shared/vercel.json" {
			t.Errorf("NameFollowingLinks() = %q, want shared/vercel.json", got)
		}
	})

	t.Run("link leaving the worktree is refused", func(t *testing.T) {
		t.Parallel()
		outside := t.TempDir()
		away := filepath.Join(outside, "vercel.json")
		if err := os.WriteFile(away, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "away.json")
		if err := os.Symlink(away, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if got, err := NameFollowingLinks(root, "away.json"); err == nil {
			t.Errorf("NameFollowingLinks() = %q, want error for a link out of the worktree", got)
		}
	})

	t.Run("dangling link reports not-exist", func(t *testing.T) {
		t.Parallel()
		link := filepath.Join(root, "dangling.json")
		if err := os.Symlink(filepath.Join(root, "missing.json"), link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		_, err := NameFollowingLinks(root, "dangling.json")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("NameFollowingLinks() error = %v, want os.ErrNotExist", err)
		}
	})
}
