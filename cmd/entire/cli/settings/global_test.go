package settings

import (
	"context"
	"errors"
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
		// Entire mirror origins canonicalize the forge prefix back to the
		// forge host, so github patterns cover mirror-origin clones too.
		{"entire://aws-us-east-2.entire.io/gh/Acme/Widgets.git", "github.com/acme/widgets"},
		// Subgroups land in the repo part: host/owner/sub.../repo.
		{"https://gitlab.com/acme/team/proj.git", "gitlab.com/acme/team/proj"},
		// Present-but-unnormalizable origins → "" (the caller fails closed).
		{"/srv/git/secret.git", ""},
		{"file:///srv/git/secret.git", ""},
		// insteadOf shorthands normalize to the shorthand form — the
		// ExcludeOrigins contract is "patterns match what git config stores".
		{"gh:acme/widgets", "gh/acme/widgets"},
		// Trailing slashes and any-case ".git" name the same clone target and
		// must not leak into the normalized form (they'd fail open: non-empty,
		// matching no pattern).
		{"https://github.com/acme/widgets/", "github.com/acme/widgets"},
		{"git@github.com:acme/widgets.GIT", "github.com/acme/widgets"},
		{"https://github.com/acme/widgets.git/", "github.com/acme/widgets"},
		{"https://github.com/acme/widgets/.git", "github.com/acme/widgets"},
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
		wantErr  bool
	}{
		{"tilde doublestar", []string{"~/oss/**"}, filepath.Join(home, "oss", "some", "repo"), true, false},
		{"bare dir excludes subtree", []string{"~/oss"}, filepath.Join(home, "oss", "repo"), true, false},
		{"exact dir", []string{"~/oss"}, filepath.Join(home, "oss"), true, false},
		{"non-match", []string{"~/oss/**"}, filepath.Join(home, "work", "repo"), false, false},
		{"absolute pattern", []string{"/tmp/scratch/**"}, "/tmp/scratch/x", true, false},
		{"trailing slash excludes subtree", []string{"/tmp/scratch/"}, "/tmp/scratch/x", true, false},
		// The subtree rule is a PATH boundary, not a string prefix: /tmp/scr
		// must never exclude /tmp/scratch (or ~/work exclude ~/work-oss).
		{"name prefix does not over-exclude", []string{"/tmp/scr"}, "/tmp/scratch", false, false},
		// Unusable patterns fail CLOSED (error → caller deactivates global
		// mode), mirroring the strict-decode rationale in LoadUserSettings: a
		// typo'd pattern must not silently track a repo the user excluded.
		{"tilde-user form fails closed", []string{"~scratch"}, "/tmp/scratch", false, true},
		{"relative pattern fails closed", []string{"tmp/scratch"}, "/tmp/scratch", false, true},
		{"invalid glob fails closed", []string{"[", "/tmp/scratch/**"}, "/tmp/scratch/x", false, true},
		// Blank entries carry no intent to honor — skipped, not an error.
		{"empty pattern skipped", []string{""}, "/tmp/scratch", false, false},
		{"whitespace-only pattern skipped", []string{"   "}, "/tmp/scratch", false, false},
		{"empty list", nil, "/anywhere", false, false},
		{"trailing whitespace trimmed", []string{"/tmp/scratch "}, "/tmp/scratch/x", true, false},
		{"tilde with trailing whitespace trimmed", []string{"~/oss "}, filepath.Join(home, "oss"), true, false},
		{"tilde with leading whitespace still expands", []string{" ~/oss"}, filepath.Join(home, "oss"), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := matchesExcludePath(t.Context(), c.patterns, c.root)
			if c.wantErr != (err != nil) {
				t.Fatalf("matchesExcludePath(%v, %q) err = %v, wantErr %v", c.patterns, c.root, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("matchesExcludePath(%v, %q) = %v, want %v", c.patterns, c.root, got, c.want)
			}
		})
	}
}

// TestMatchesExcludePath_WindowsDriveAbsolute pins that drive-letter
// absolutes pass the relative-pattern guard: filepath.IsAbs, not the "/"
// prefix (which they can never carry), is what qualifies them.
func TestMatchesExcludePath_WindowsDriveAbsolute(t *testing.T) {
	t.Parallel()
	// Gate on the capability itself: drive-letter paths are absolute only on
	// Windows, which is exactly the property under test.
	if !filepath.IsAbs(`C:/code`) {
		t.Skip("drive-letter absolutes exist only on Windows")
	}
	matched, err := matchesExcludePath(t.Context(), []string{`C:/code/**`}, `C:\code\repo`)
	if err != nil {
		t.Fatalf("drive-letter absolute pattern must not be rejected as relative: %v", err)
	}
	if !matched {
		t.Error(`C:/code/** must exclude C:\code\repo`)
	}
	// The MSYS spelling (/c/code/**) is NOT absolute on Windows and can never
	// match a drive-rooted root — it must fail closed, not silently no-op.
	if _, err := matchesExcludePath(t.Context(), []string{`/c/code/**`}, `C:\code\repo`); err == nil {
		t.Error("slash-rooted pattern must fail closed on Windows (it can never match)")
	}
}

// TestMatchesExcludePath_SymlinkedPrefix pins the logical/physical bridging:
// a pattern written against a symlinked directory (the logical ~/code) must
// exclude a worktree root reported in physical form, and vice versa — git
// reports physical paths while patterns are typed against logical ones.
func TestMatchesExcludePath_SymlinkedPrefix(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir()) // canonical base (macOS /var → /private/var)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(base, "physical")
	repo := filepath.Join(physical, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(base, "logical")
	if err := os.Symlink(physical, logical); err != nil {
		t.Fatal(err)
	}

	t.Run("logical pattern excludes physical root", func(t *testing.T) {
		t.Parallel()
		matched, err := matchesExcludePath(t.Context(), []string{filepath.ToSlash(logical) + "/**"}, repo)
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			t.Error("a pattern through the symlink must exclude the physical worktree root")
		}
	})
	t.Run("physical pattern excludes logical root", func(t *testing.T) {
		t.Parallel()
		matched, err := matchesExcludePath(t.Context(), []string{filepath.ToSlash(physical) + "/**"}, filepath.Join(logical, "repo"))
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			t.Error("a physical pattern must exclude a root reached through the symlink")
		}
	})
}

func TestMatchesExcludePathFold(t *testing.T) {
	t.Parallel()
	patterns := []string{"/TMP/Scratch/**"}
	matched, err := matchesExcludePathFold(t.Context(), patterns, "/tmp/scratch/x", false)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Error("without folding, a differently-cased pattern must not match")
	}
	matched, err = matchesExcludePathFold(t.Context(), patterns, "/tmp/scratch/x", true)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Error("with folding, a differently-cased pattern must match")
	}
}

func TestMatchesExcludePathExact(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := []struct {
		name    string
		entries []string
		root    string
		want    bool
		wantErr bool
	}{
		{"exact match excludes", []string{"/tmp/scratch"}, "/tmp/scratch", true, false},
		// The defining difference from exclude_paths: no subtree cascade.
		{"child repo of excluded-exact path is NOT excluded", []string{"/tmp/scratch"}, "/tmp/scratch/repo", false, false},
		{"tilde expands to home", []string{"~"}, home, true, false},
		{"tilde child not excluded by bare ~", []string{"~"}, filepath.Join(home, "oss", "repo"), false, false},
		{"tilde path expands", []string{"~/dotfiles"}, filepath.Join(home, "dotfiles"), true, false},
		{"trailing slash cleaned", []string{"/tmp/scratch/"}, "/tmp/scratch", true, false},
		// Entries are plain paths: glob meta characters have no meaning.
		{"glob chars are literal", []string{"/tmp/scratch/*"}, "/tmp/scratch/x", false, false},
		// Unusable entries fail CLOSED, same discipline as exclude_paths.
		{"relative entry fails closed", []string{"tmp/scratch"}, "/tmp/scratch", false, true},
		{"tilde-user form fails closed", []string{"~scratch"}, "/tmp/scratch", false, true},
		{"blank entry skipped", []string{"", "   "}, "/tmp/scratch", false, false},
		{"empty list", nil, "/anywhere", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := matchesExcludePathExact(t.Context(), c.entries, c.root)
			if c.wantErr != (err != nil) {
				t.Fatalf("matchesExcludePathExact(%v, %q) err = %v, wantErr %v", c.entries, c.root, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("matchesExcludePathExact(%v, %q) = %v, want %v", c.entries, c.root, got, c.want)
			}
		})
	}
}

func TestMatchesExcludePathExactFold(t *testing.T) {
	t.Parallel()
	entries := []string{"/TMP/Scratch"}
	matched, err := matchesExcludePathExactFold(t.Context(), entries, "/tmp/scratch", false)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Error("without folding, a differently-cased entry must not match")
	}
	matched, err = matchesExcludePathExactFold(t.Context(), entries, "/tmp/scratch", true)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Error("with folding, a differently-cased entry must match")
	}
}

func TestMatchesExcludeOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		patterns []string
		origin   string // already normalized
		want     bool
		wantErr  bool
	}{
		{"owner wildcard", []string{"github.com/acme/*"}, "github.com/acme/widgets", true, false},
		{"exact", []string{"github.com/acme/widgets"}, "github.com/acme/widgets", true, false},
		{"different owner", []string{"github.com/acme/*"}, "github.com/other/widgets", false, false},
		// Subgroup hosts: `*` does not cross `/`, `**` does — documented on
		// ExcludeOrigins. A gitlab.com/acme/* pattern does NOT cover the
		// namespace; that silence is why the doc steers users to `**`.
		{"owner wildcard does not cross subgroups", []string{"gitlab.com/acme/*"}, "gitlab.com/acme/team/proj", false, false},
		{"doublestar covers subgroups", []string{"gitlab.com/acme/**"}, "gitlab.com/acme/team/proj", true, false},
		{"no origin matches nothing", []string{"github.com/acme/*"}, "", false, false},
		{"empty list", nil, "github.com/acme/widgets", false, false},
		{"trailing whitespace trimmed", []string{"github.com/acme/* "}, "github.com/acme/widgets", true, false},
		{"whitespace-only pattern skipped", []string{"   "}, "github.com/acme/widgets", false, false},
		{"invalid glob fails closed", []string{"["}, "github.com/acme/widgets", false, true},
		{"invalid glob fails closed even with no origin", []string{"["}, "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := matchesExcludeOrigin(t.Context(), c.patterns, c.origin)
			if c.wantErr != (err != nil) {
				t.Fatalf("matchesExcludeOrigin(%v, %q) err = %v, wantErr %v", c.patterns, c.origin, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("matchesExcludeOrigin(%v, %q) = %v, want %v", c.patterns, c.origin, got, c.want)
			}
		})
	}
}

// newGlobalTestRepo initializes an isolated repo for IsActiveForRepo tests.
func newGlobalTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	return dir
}

// addTestRemote wires a remote URL with the repo's git config isolated from
// the developer's global config (an insteadOf rewrite there could otherwise
// alter what `git config` stores and flake these tests).
func addTestRemote(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestGlobalModeActive_FastPathNeverResolvesWorktreeRoot pins the gate's
// fork-avoidance invariant: with the global tier unconfigured or disabled —
// the machine-wide common case, hit on every hook invocation in every repo
// without repo-level setup — GlobalModeActive must answer from the settings
// file alone, never resolving the worktree root (a `git rev-parse`
// subprocess fork).
// No t.Parallel: swaps the package-level worktreeRootFn seam and uses
// t.Setenv; every other caller of the gate is serial for the same reason.
func TestGlobalModeActive_FastPathNeverResolvesWorktreeRoot(t *testing.T) {
	restore := SetWorktreeRootFnForTesting(func(context.Context) (string, error) {
		t.Error("worktree root resolved on the unconfigured/disabled fast path — this forks git on every hook invocation")
		return "", errors.New("forbidden fork")
	})
	t.Cleanup(restore)

	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir()) // unconfigured tier
	ClearGlobalModeCache()                     // a stale memo entry would bypass the path under test
	t.Cleanup(ClearGlobalModeCache)
	if GlobalModeActive(t.Context()) {
		t.Fatal("unconfigured tier must be inactive")
	}

	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeUserSettings(t, cfg, `{"global":{"enabled":false}}`) // recorded "no"
	ClearGlobalModeCache()
	if GlobalModeActive(t.Context()) {
		t.Fatal("disabled tier must be inactive")
	}
}

func TestIsActiveForRepo(t *testing.T) {
	newRepo := newGlobalTestRepo

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

	t.Run("local-only repo enable is active", func(t *testing.T) {
		dir := newRepo(t)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		// settings.local.json ONLY, enabled — `enable --local` writes just this
		// file, and the gate treating it as inactive silently drops all capture.
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.local.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if !IsActiveForRepo(t.Context()) {
			t.Fatal("a local-only enabled setup must activate")
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

	t.Run("exact path exclusion", func(t *testing.T) {
		dir := newRepo(t)
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_paths_exact":["`+filepath.ToSlash(resolved)+`"]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("a worktree root listed in exclude_paths_exact must not activate")
		}
	})

	t.Run("exact path exclusion does not cascade to child repos", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "repo")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		testutil.InitRepo(t, dir)
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			t.Fatal(err)
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_paths_exact":["`+filepath.ToSlash(resolvedParent)+`"]}}`)
		t.Chdir(dir)
		if !IsActiveForRepo(t.Context()) {
			t.Fatal("exclude_paths_exact must not exclude repos beneath the listed path")
		}
	})

	t.Run("origin exclusion end-to-end", func(t *testing.T) {
		dir := newRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "git@github.com:Acme/Widgets.git")
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
		addTestRemote(t, dir, "remote", "add", "origin", "git@gitlab.example.com:team/proj.git")
		addTestRemote(t, dir, "remote", "set-url", "--add", "origin", "git@github.com:Acme/Widgets.git")
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

	t.Run("local-only repo disable vetoes global", func(t *testing.T) {
		dir := newRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		// settings.local.json ONLY — `enable --local` opt-out must be final,
		// exactly like a base settings.json veto.
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.local.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("a local-only repo-level disable must veto global mode")
		}
	})
}

// TestIsActiveForRepo_FailClosed groups the failure-mode legs of the gate
// predicate: every error path — unreadable or malformed settings at either
// tier, an unresolvable worktree, an unusable exclude pattern, an origin
// that cannot be normalized — must deactivate rather than guess.
func TestIsActiveForRepo_FailClosed(t *testing.T) {
	t.Run("corrupt repo settings with global on", func(t *testing.T) {
		dir := newGlobalTestRepo(t)
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

	t.Run("malformed global file", func(t *testing.T) {
		dir := newGlobalTestRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{broken`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("malformed global settings must fail closed")
		}
	})

	t.Run("unreadable global file", func(t *testing.T) {
		dir := newGlobalTestRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		// settings.json as a DIRECTORY exercises the read-error branch (as
		// opposed to the decode-error branch above) portably.
		if err := os.MkdirAll(filepath.Join(cfg, UserSettingsFileName), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("an unreadable global settings file must fail closed")
		}
	})

	t.Run("global on outside any git repo", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		t.Chdir(t.TempDir()) // plain directory, no repo
		if IsActiveForRepo(t.Context()) {
			t.Fatal("global mode must not activate outside a git repository (worktree unresolvable)")
		}
	})

	t.Run("unusable exclude pattern", func(t *testing.T) {
		dir := newGlobalTestRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		// Relative pattern — a natural mistake (every other glob tool is
		// repo-relative). It can never match, so honoring it is impossible;
		// global mode must deactivate rather than track the repo.
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_paths":["code/**"]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("an unusable exclude pattern must deactivate global mode (fail closed)")
		}
	})

	t.Run("malformed exclude_origins pattern, repo has no origin", func(t *testing.T) {
		dir := newGlobalTestRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_origins":["["]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("a malformed exclude_origins pattern must fail closed identically with and without an origin")
		}
	})

	t.Run("blank origin value", func(t *testing.T) {
		dir := newGlobalTestRepo(t)
		// `url =` with no value: the key exists but exclusion cannot be checked.
		addTestRemote(t, dir, "config", "remote.origin.url", "")
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_origins":["github.com/acme/*"]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("a blank origin URL must read as present-but-uncheckable, not as no-origin")
		}
	})

	t.Run("unparseable present origin", func(t *testing.T) {
		dir := newGlobalTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "/srv/git/secret.git")
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_origins":["github.com/acme/*"]}}`)
		t.Chdir(dir)
		if IsActiveForRepo(t.Context()) {
			t.Fatal("an origin that is present but cannot be normalized means exclusion could not be checked — must fail closed")
		}
	})
}
