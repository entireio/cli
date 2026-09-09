package settings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A settings file is never read through a symbolic link, and where the link
// points makes no difference. The `.entire` guard in the cli package covers the
// commands that go through the root pre-run, but eighteen files call
// settings.Load directly — every strategy hook path among them — so the
// invariant has to hold here too, at the read itself.
//
// os.OpenRoot, which readConfined already opens through, rejects only a link
// that leaves `.entire`. That is why the in-directory rows below exist: they are
// the cases confinement follows without complaint.
func TestLoad_RejectsSymlinkedSettingsFile(t *testing.T) {
	for _, name := range []string{"settings.json", "settings.local.json"} {
		for _, target := range []string{"relative-inside", "absolute-inside", "outside", "escaping", "dangling"} {
			t.Run(name+"/"+target, func(t *testing.T) {
				root := t.TempDir()
				testutil.InitRepo(t, root)
				entireDir := filepath.Join(root, ".entire")
				if err := os.MkdirAll(entireDir, 0o750); err != nil {
					t.Fatal(err)
				}

				var linkTarget string
				switch target {
				case "relative-inside":
					// The case os.Root confinement follows without complaint.
					if err := os.WriteFile(filepath.Join(entireDir, "planted.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
						t.Fatal(err)
					}
					linkTarget = "planted.json"
				case "absolute-inside":
					linkTarget = filepath.Join(entireDir, "planted.json")
					if err := os.WriteFile(linkTarget, []byte(`{"enabled":false}`), 0o600); err != nil {
						t.Fatal(err)
					}
				case "outside":
					linkTarget = filepath.Join(t.TempDir(), "planted.json")
					if err := os.WriteFile(linkTarget, []byte(`{"enabled":false}`), 0o600); err != nil {
						t.Fatal(err)
					}
				case "escaping":
					if err := os.WriteFile(filepath.Join(root, "planted.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
						t.Fatal(err)
					}
					linkTarget = "../planted.json"
				case "dangling":
					// Previously ENOENT, which every caller reads as "absent"
					// and answers with default settings.
					linkTarget = "missing.json"
				}
				if err := os.Symlink(linkTarget, filepath.Join(entireDir, name)); err != nil {
					t.Skipf("cannot create symlink: %v", err)
				}

				_, err := Load(WithWorktreeRoot(context.Background(), root))
				if err == nil {
					t.Fatalf("Load read %s through a symbolic link", name)
				}
				if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
					t.Errorf("error does not wrap ErrEntireDirUnsupportedEntry: %v", err)
				}
				if msg := err.Error(); !strings.Contains(msg, name) {
					t.Errorf("message %q does not name the offending file", msg)
				}
			})
		}
	}
}

// The refusal has to be the whole outcome, not a fallback to defaults: a
// redirected settings.local.json saying `enabled: false` must not be able to
// turn Entire off, and neither may the error be swallowed into a default-on
// settings object that looks the same as a healthy repo.
func TestLoad_SymlinkedLocalSettingsAreNotHonoured(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	entireDir := filepath.Join(root, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "planted.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("planted.json", filepath.Join(entireDir, "settings.local.json")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := Load(WithWorktreeRoot(context.Background(), root))
	if err == nil {
		t.Fatalf("Load honoured a redirected settings.local.json (enabled=%v)", got.Enabled)
	}
	if got != nil {
		t.Errorf("Load returned settings alongside the error: %+v", got)
	}
}

// The single-file readers serve `entire status` and the raw read-modify-write
// halves. They read the same files, so they refuse the same links; LoadLocalRaw
// is ungated for trackedness but that is a different question from this one.
func TestSingleFileReaders_RejectSymlinkedSettingsFile(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	entireDir := filepath.Join(root, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "planted.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if err := os.Symlink("planted.json", filepath.Join(entireDir, name)); err != nil {
			t.Skipf("cannot create symlink: %v", err)
		}
	}

	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	t.Run("LoadFromFile", func(t *testing.T) {
		_, err := LoadFromFile(filepath.Join(entireDir, "settings.json"))
		if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
			t.Fatalf("want ErrEntireDirUnsupportedEntry, got %v", err)
		}
	})

	t.Run("LoadProjectRaw", func(t *testing.T) {
		_, _, _, err := LoadProjectRaw(context.Background())
		if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
			t.Fatalf("want ErrEntireDirUnsupportedEntry, got %v", err)
		}
	})

	t.Run("LoadLocalRaw", func(t *testing.T) {
		_, _, _, err := LoadLocalRaw(context.Background())
		if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
			t.Fatalf("want ErrEntireDirUnsupportedEntry, got %v", err)
		}
	})
}

// Clone preferences live in the git common dir rather than `.entire`, but they
// are Entire's own state read through the same chokepoint, so they get the same
// treatment. Worth pinning: the check sits in readConfined, so this follows from
// where it was put rather than from a decision made at this call site.
func TestLoadClonePreferences_RejectsSymlink(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)

	path, err := clonePreferencesPathForWorktreeRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "planted.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("planted.json", path); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	if _, err := Load(WithWorktreeRoot(context.Background(), root)); !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
		t.Fatalf("want ErrEntireDirUnsupportedEntry, got %v", err)
	}
}

// Real files must keep loading. Without this the check is one mistake away from
// refusing every settings read in every repo.
func TestLoad_RealSettingsFilesStillLoad(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	entireDir := filepath.Join(root, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(`{"absolute_git_hook_path":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(WithWorktreeRoot(context.Background(), root))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Enabled || !got.AbsoluteGitHookPath {
		t.Errorf("both layers did not merge: enabled=%v absolute_git_hook_path=%v",
			got.Enabled, got.AbsoluteGitHookPath)
	}
}

// The Lstat/Open race that used to be modelled here is now covered where the
// check lives: osroot.TestValidateOpenedFile_RejectsReplacedEntry. readConfinedIn
// delegates to osroot.ReadFileNoFollow rather than carrying a second copy of it.
