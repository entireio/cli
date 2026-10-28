package userdirs

import (
	"os/user"
	"path/filepath"
	"runtime"
	"testing"
)

// Item 5's fix, at the layer that owns the distinction. A user-supplied
// override that is relative is refused; Entire's own home-failure fallback is
// absolutized instead, because there is nothing for the user to fix and a
// cwd-relative directory that works beats a command that does not.
//
// ownFallbackDir is exercised directly: os.UserHomeDir cannot be made to fail
// portably from a test (Windows consults the API, not $HOME) and testdirs
// intercepts the fallback under `go test` anyway, so the resolver's branch is
// unreachable in-process. What matters is the property, which is that the
// fallback it returns is absolute.
func TestOwnFallbackDir_IsAbsoluteSoConsumersAccept(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	got := ownFallbackDir(".config", "entire")
	if !filepath.IsAbs(got) {
		t.Fatalf("ownFallbackDir() = %q, want an absolute path: every consumer refuses a "+
			"relative directory, and refusing Entire's own fallback broke every command "+
			"that touches a saved login on a machine with no resolvable home", got)
	}
	if want := filepath.Join(cwd, ".config", "entire"); got != want {
		t.Errorf("ownFallbackDir() = %q, want %q", got, want)
	}
	// It must satisfy the very check the consumers apply.
	if err := RequireAbsoluteOverride("config dir", got); err != nil {
		t.Errorf("the fallback must pass the consumers' own check, got %v", err)
	}
}

// The distinction is not softened: a relative value the USER set is still
// refused, in both variables.
func TestRelativeUserOverridesAreStillRefused(t *testing.T) {
	for _, tc := range []struct{ env, val string }{
		{EnvConfigDir, "relative-config"},
		{EnvCacheHome, "relative-cache"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			var err error
			if tc.env == EnvConfigDir {
				_, err = configDir()
			} else {
				_, err = cacheDir()
			}
			if err == nil {
				t.Fatalf("%s=%q was accepted; a user override must be absolute", tc.env, tc.val)
			}
		})
	}
}

// Root and unset HOME resolve via passwd.
func TestHome_PasswdForRootOrUnsetHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows keeps os.UserHomeDir")
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no passwd entry: %v", err)
	}
	elsewhere := t.TempDir()

	t.Run("user keeps HOME", func(t *testing.T) {
		setEuid(t, 1000)
		t.Setenv("HOME", elsewhere)
		got, err := Home()
		if err != nil || got != elsewhere {
			t.Errorf("Home() = %q, %v; want %q", got, err, elsewhere)
		}
	})
	t.Run("unset HOME uses passwd", func(t *testing.T) {
		setEuid(t, 1000)
		t.Setenv("HOME", "")
		got, err := Home()
		if err != nil || got != u.HomeDir {
			t.Errorf("Home() = %q, %v; want %q", got, err, u.HomeDir)
		}
	})
	t.Run("root ignores HOME", func(t *testing.T) {
		setEuid(t, 0)
		t.Setenv("HOME", elsewhere)
		got, err := Home()
		if err != nil || got != u.HomeDir {
			t.Errorf("Home() = %q, %v; want %q", got, err, u.HomeDir)
		}
	})
}

func setEuid(t *testing.T, uid int) {
	t.Helper()
	orig := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = orig })
}
