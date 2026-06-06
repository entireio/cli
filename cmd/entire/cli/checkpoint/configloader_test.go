package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-git/go-billy/v6/osfs"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	xconfig "github.com/go-git/go-git/v6/x/plugin/config"
)

// writeSymlinkedGlobalConfig builds a fake $HOME whose XDG config directory
// (~/.config) is an *absolute* symlink to a directory elsewhere — exactly what
// dotfile managers like chezmoi, GNU Stow, or yadm produce. The global git
// config lives at ~/.config/git/config (reached only when ~/.gitconfig is
// absent). It returns the fake home directory. It skips on platforms where
// symlink creation is unprivileged/unsupported.
func writeSymlinkedGlobalConfig(t *testing.T, contents string) (home string) {
	t.Helper()

	base := t.TempDir()
	home = filepath.Join(base, "home")
	realConfig := filepath.Join(base, "dotfiles", "config")

	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realConfig, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realConfig, "git", "config"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	// ~/.config -> absolute path. os.Root rejects this even though it resolves
	// back inside the host root, which is the root cause of the bug.
	if err := os.Symlink(realConfig, filepath.Join(home, ".config")); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	return home
}

// pointHomeAt isolates global git config resolution onto home: it sets HOME,
// disables system config, and forces XDG resolution onto ~/.config by clearing
// XDG_CONFIG_HOME and GIT_CONFIG_GLOBAL (the latter is unset, not emptied,
// since an empty value disables global config entirely).
func pointHomeAt(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// t.Setenv registers restoration of the original value; unset it for the
	// test so go-git falls back to XDG (an empty value disables global config).
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	if err := os.Unsetenv("GIT_CONFIG_GLOBAL"); err != nil {
		t.Fatal(err)
	}
}

// TestOSSymlinkFS_ReadsGlobalConfigBehindSymlink reproduces the customer's
// "path escapes from parent" failure and verifies osSymlinkFS fixes it.
//
// The default loader (osfs.Default, backed by os.Root) must fail; the
// symlink-following loader must read the config.
func TestOSSymlinkFS_ReadsGlobalConfigBehindSymlink(t *testing.T) {
	// Cannot t.Parallel(): mutates HOME/XDG/GIT_CONFIG_* via t.Setenv.
	if runtime.GOOS == "windows" {
		t.Skip("XDG default path and symlink semantics differ on Windows")
	}

	const cfg = "[user]\n\tname = Jag\n\temail = jag@example.com\n"
	home := writeSymlinkedGlobalConfig(t, cfg)
	pointHomeAt(t, home)

	// Default loader (os.Root): must reject the symlinked path, matching the
	// customer's report. This locks in the regression.
	if _, err := xconfig.NewAuto().Load(config.GlobalScope); err == nil {
		t.Fatal("default osfs loader unexpectedly succeeded on symlinked XDG config; regression no longer reproduces")
	} else if !errors.Is(err, osfs.ErrPathEscapesParent) {
		t.Fatalf("default loader error = %v, want errors.Is(..., osfs.ErrPathEscapesParent)", err)
	}

	// Symlink-following loader: must read through the symlink like git does.
	storer, err := xconfig.NewAuto(xconfig.WithFilesystem(osSymlinkFS{})).Load(config.GlobalScope)
	if err != nil {
		t.Fatalf("osSymlinkFS loader failed to load global config: %v", err)
	}
	loaded, err := storer.Config()
	if err != nil {
		t.Fatalf("storer.Config() error = %v", err)
	}
	if loaded.User.Name != "Jag" {
		t.Errorf("user.name = %q, want %q", loaded.User.Name, "Jag")
	}
	if loaded.User.Email != "jag@example.com" {
		t.Errorf("user.email = %q, want %q", loaded.User.Email, "jag@example.com")
	}
}

// TestGetGitAuthorFromRepo_GlobalConfigBehindSymlink verifies the user-facing
// impact is fixed: with the symlink-following loader registered, checkpoint
// commit authorship resolves from a global config behind a symlinked ~/.config
// instead of falling back to "Unknown".
func TestGetGitAuthorFromRepo_GlobalConfigBehindSymlink(t *testing.T) {
	// Cannot t.Parallel(): mutates HOME/XDG/GIT_CONFIG_* and the plugin registry.
	if runtime.GOOS == "windows" {
		t.Skip("XDG default path and symlink semantics differ on Windows")
	}

	useSymlinkConfigLoader(t)

	home := writeSymlinkedGlobalConfig(t, "[user]\n\tname = Jag\n\temail = jag@example.com\n")
	pointHomeAt(t, home)

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	name, email := GetGitAuthorFromRepo(repo)
	if name != "Jag" {
		t.Errorf("name = %q, want %q", name, "Jag")
	}
	if email != "jag@example.com" {
		t.Errorf("email = %q, want %q", email, "jag@example.com")
	}
}

// useSymlinkConfigLoader registers the production symlink-following config
// loader (osSymlinkFS) for the duration of t, restoring NewEmpty afterwards so
// other tests stay isolated from the host environment.
func useSymlinkConfigLoader(t *testing.T) {
	t.Helper()
	registerConfigLoaderForTest(t, registerSymlinkConfigLoader)
}
