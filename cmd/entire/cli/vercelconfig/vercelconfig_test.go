package vercelconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestLoadIn(t *testing.T) {
	t.Parallel()

	t.Run("absent file yields an empty config", func(t *testing.T) {
		t.Parallel()
		config, disabled, err := LoadIn(openRoot(t, t.TempDir()), FileName)
		if err != nil {
			t.Fatalf("LoadIn: %v", err)
		}
		if len(config) != 0 || disabled {
			t.Errorf("config = %v, disabled = %v", config, disabled)
		}
	})

	t.Run("reads a config and reports whether Entire branches are disabled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		body := []byte(`{"git":{"deploymentEnabled":{"` + BranchPattern + `":false}}}`)
		if err := os.WriteFile(filepath.Join(dir, FileName), body, 0o600); err != nil {
			t.Fatal(err)
		}
		config, disabled, err := LoadIn(openRoot(t, dir), FileName)
		if err != nil {
			t.Fatalf("LoadIn: %v", err)
		}
		if config == nil {
			t.Fatal("config is nil")
		}
		if !disabled {
			t.Error("deployment should read as disabled")
		}
	})

	t.Run("malformed JSON is an error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadIn(openRoot(t, dir), FileName); err == nil {
			t.Error("want a parse error")
		}
	})

	// vercel.json arrives with the checkout, so its size is not ours to trust.
	t.Run("refuses a file over the cap", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		body := `{"padding":"` + strings.Repeat("x", maxConfigBytes) + `"}`
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := LoadIn(openRoot(t, dir), FileName)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("err = %v, want an over-cap error", err)
		}
	})

	// The containment the root exists for: a link that stays inside the
	// repository is a real monorepo setup and is followed, one leaving it is not.
	t.Run("symlinks", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		outside := t.TempDir()

		if err := os.WriteFile(filepath.Join(dir, "shared.json"), []byte(`{"inside":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("shared.json", filepath.Join(dir, FileName)); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		config, _, err := LoadIn(openRoot(t, dir), FileName)
		if err != nil {
			t.Fatalf("an in-repo link must still be followed: %v", err)
		}
		if config["inside"] != true {
			t.Errorf("config = %v", config)
		}

		escaping := filepath.Join(outside, "escape.json")
		if err := os.WriteFile(escaping, []byte(`{"outside":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(escaping, filepath.Join(dir, "vercel.escaping.json")); err != nil {
			t.Fatal(err)
		}
		if got, _, err := LoadIn(openRoot(t, dir), "vercel.escaping.json"); err == nil {
			t.Errorf("LoadIn followed a link out of the worktree: %v", got)
		}
	})
}

// TestLoadIn_AbsoluteInRepoSymlink documents what a root alone cannot do, which
// is why the caller resolves the name first. os.Root refuses an absolute
// symlink target unconditionally — even one landing inside the root — so
// pointing vercel.json at a monorepo's shared config with an absolute link
// reaches LoadIn as an error that is NOT os.ErrNotExist.
func TestLoadIn_AbsoluteInRepoSymlink(t *testing.T) {
	t.Parallel()

	testutil.SkipWithoutSymlinks(t)

	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(shared, FileName)
	if err := os.WriteFile(target, []byte(`{"git":{"deploymentEnabled":{"entire/**":false}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Absolute, and resolving inside the worktree.
	if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	// The raw name is refused, and not as "absent" — which is what made this
	// silently drop the feature rather than fall through.
	_, _, err = LoadIn(root, FileName)
	if err == nil {
		t.Fatalf("LoadIn(%q) succeeded; os.Root is expected to refuse an absolute link", FileName)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("LoadIn error = %v, want something other than os.ErrNotExist", err)
	}

	// The resolved name is what the caller passes, and it reads.
	config, disabled, err := LoadIn(root, "shared/"+FileName)
	if err != nil {
		t.Fatalf("LoadIn(resolved) error = %v", err)
	}
	if config == nil || !disabled {
		t.Errorf("LoadIn(resolved) = %v, disabled=%v; want the shared config read", config, disabled)
	}
}
