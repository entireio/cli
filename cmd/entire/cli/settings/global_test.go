package settings

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func writeUserSettings(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, UserSettingsFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUserSettings_MissingFileIsUnconfigured(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	us, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatalf("missing file must not error, got %v", err)
	}
	if us.Global != nil {
		t.Fatalf("missing file must mean unconfigured, got %+v", us.Global)
	}
}

func TestLoadUserSettings_ValidFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	writeUserSettings(t, dir, `{"global":{"enabled":true,"exclude_paths":["~/oss/**"],"exclude_origins":["github.com/acme/*"]}}`)
	us, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if us.Global == nil || !us.Global.Enabled {
		t.Fatalf("expected enabled global config, got %+v", us.Global)
	}
	if len(us.Global.ExcludePaths) != 1 || len(us.Global.ExcludeOrigins) != 1 {
		t.Fatalf("exclude lists not parsed: %+v", us.Global)
	}
}

func TestLoadUserSettings_MalformedFileErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	writeUserSettings(t, dir, `{not json`)
	if _, err := LoadUserSettings(t.Context()); err == nil {
		t.Fatal("malformed file must return error (consumers fail closed)")
	}
}

func TestLoadUserSettings_UnknownFieldErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	writeUserSettings(t, dir, `{"global":{"enabled":true,"exclud_paths":["~/oss"]}}`)
	if _, err := LoadUserSettings(t.Context()); err == nil {
		t.Fatal("typo'd key must return error, not silently drop the exclude list")
	}
}

func TestNormalizeOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"git@github.com:Acme/Widgets.git", "github.com/acme/widgets"},
		{"https://github.com/acme/widgets", "github.com/acme/widgets"},
		{"https://github.com/acme/widgets.git", "github.com/acme/widgets"},
		{"ssh://git@gitlab.example.com/team/proj.git", "gitlab.example.com/team/proj"},
		{"not a url at all", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeOrigin(c.in); got != c.want {
			t.Errorf("normalizeOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatchesExcludePath(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := []struct {
		name     string
		patterns []string
		root     string
		want     bool
	}{
		{"tilde doublestar", []string{"~/oss/**"}, filepath.Join(home, "oss", "some", "repo"), true},
		{"bare dir excludes subtree", []string{"~/oss"}, filepath.Join(home, "oss", "repo"), true},
		{"exact dir", []string{"~/oss"}, filepath.Join(home, "oss"), true},
		{"non-match", []string{"~/oss/**"}, filepath.Join(home, "work", "repo"), false},
		{"absolute pattern", []string{"/tmp/scratch/**"}, "/tmp/scratch/x", true},
		{"trailing slash excludes subtree", []string{"/tmp/scratch/"}, "/tmp/scratch/x", true},
		{"tilde-user form skipped", []string{"~scratch"}, "/tmp/scratch", false},
		{"empty pattern skipped", []string{""}, "/tmp/scratch", false},
		{"relative pattern skipped", []string{"tmp/scratch"}, "/tmp/scratch", false},
		{"invalid glob skipped, valid still applies", []string{"[", "/tmp/scratch/**"}, "/tmp/scratch/x", true},
		{"empty list", nil, "/anywhere", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := matchesExcludePath(t.Context(), c.patterns, c.root); got != c.want {
				t.Errorf("matchesExcludePath(%v, %q) = %v, want %v", c.patterns, c.root, got, c.want)
			}
		})
	}
}

func TestMatchesExcludePathFold(t *testing.T) {
	t.Parallel()
	patterns := []string{"/TMP/Scratch/**"}
	if matchesExcludePathFold(t.Context(), patterns, "/tmp/scratch/x", false) {
		t.Error("without folding, a differently-cased pattern must not match")
	}
	if !matchesExcludePathFold(t.Context(), patterns, "/tmp/scratch/x", true) {
		t.Error("with folding, a differently-cased pattern must match")
	}
}

func TestMatchesExcludeOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		patterns []string
		origin   string // already normalized
		want     bool
	}{
		{"owner wildcard", []string{"github.com/acme/*"}, "github.com/acme/widgets", true},
		{"exact", []string{"github.com/acme/widgets"}, "github.com/acme/widgets", true},
		{"different owner", []string{"github.com/acme/*"}, "github.com/other/widgets", false},
		{"no origin matches nothing", []string{"github.com/acme/*"}, "", false},
		{"empty list", nil, "github.com/acme/widgets", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := matchesExcludeOrigin(t.Context(), c.patterns, c.origin); got != c.want {
				t.Errorf("matchesExcludeOrigin(%v, %q) = %v, want %v", c.patterns, c.origin, got, c.want)
			}
		})
	}
}

func TestIsActiveForRepo(t *testing.T) {
	newRepo := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		return dir
	}

	t.Run("no setup, global on", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		t.Chdir(dir)
		if !IsActiveForRepo(t.Context()) {
			t.Fatal("un-set-up repo with global on must be active")
		}
	})

	t.Run("no setup, global off", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":false}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("global disabled must not activate")
		}
	})

	t.Run("no setup, global unconfigured", func(t *testing.T) {
		dir := newRepo(t)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("unconfigured global must not activate")
		}
	})

	t.Run("explicit repo disable vetoes global", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("explicit repo-level disable must veto global mode")
		}
	})

	t.Run("repo-level enabled still wins without global", func(t *testing.T) {
		dir := newRepo(t)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if !IsActiveForRepo(t.Context()) {
			t.Fatal("repo-level enabled must be active regardless of global tier")
		}
	})

	t.Run("path exclusion", func(t *testing.T) {
		dir := newRepo(t)
		// Required on macOS: t.Chdir resolves symlinks (/var/... →
		// /private/var/...), so the pattern must be built from the
		// resolved path or it never matches the worktree root.
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_paths":["`+filepath.ToSlash(resolved)+`"]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("excluded path must not activate")
		}
	})

	t.Run("origin exclusion end-to-end", func(t *testing.T) {
		dir := newRepo(t)
		cmd := exec.CommandContext(t.Context(), "git", "remote", "add", "origin", "git@github.com:Acme/Widgets.git")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git remote add: %v\n%s", err, out)
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_origins":["github.com/acme/*"]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("excluded origin must not activate (case-insensitive, ssh-form normalized)")
		}
	})

	t.Run("origin exclusion matches any configured URL", func(t *testing.T) {
		dir := newRepo(t)
		for _, args := range [][]string{
			{"remote", "add", "origin", "git@gitlab.example.com:team/proj.git"},
			{"remote", "set-url", "--add", "origin", "git@github.com:Acme/Widgets.git"},
		} {
			cmd := exec.CommandContext(t.Context(), "git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_origins":["github.com/acme/*"]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("a match on the second configured URL must still exclude")
		}
	})

	t.Run("exclude_origins set, repo has no origin", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_origins":["github.com/acme/*"]}}`)
		t.Chdir(dir)
		if !IsActiveForRepo(t.Context()) {
			t.Fatal("a repo without an origin remote matches no origin pattern and must stay active")
		}
	})

	t.Run("corrupt repo settings with global on fails closed", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{broken`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("corrupt repo-level settings must fail closed, never fall through to global")
		}
	})

	t.Run("malformed global file fails closed", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{broken`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("malformed global settings must fail closed")
		}
	})
}
