package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

// No t.Parallel in this file: every test uses t.Chdir and/or t.Setenv.

func runCheckGlobalTracking(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	checkGlobalTracking(cmd)
	return out.String()
}

func TestCheckGlobalTracking_SilentWhileOffOrUnconfigured(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)

	if got := runCheckGlobalTracking(t); got != "" {
		t.Errorf("unconfigured tier must be silent, got: %q", got)
	}

	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":false}}`)
	if got := runCheckGlobalTracking(t); got != "" {
		t.Errorf("off tier must be silent, got: %q", got)
	}
}

func TestCheckGlobalTracking_WarnsOnMissingUserHooks(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "USER-LEVEL AGENT HOOKS MISSING") {
		t.Fatalf("missing warning, got: %q", got)
	}
	for _, want := range []string{"claude-code", "gemini", "entire enable --global"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning missing %q, got: %q", want, got)
		}
	}
}

func TestCheckGlobalTracking_OKWhenUserHooksInstalled(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)

	// Install for every user-hook-capable agent, as enable --global would.
	var buf bytes.Buffer
	installUserAgentHooks(t.Context(), &buf)

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "✓ Global tracking: user-level agent hooks OK") {
		t.Fatalf("expected OK line, got: %q", got)
	}
}

func TestCheckGlobalTracking_WarnsOnMarkedCloneWithoutGitHooks(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	var buf bytes.Buffer
	installUserAgentHooks(t.Context(), &buf)

	// Mark this clone as lazily enabled without installing its git hooks.
	if err := settings.ModifyClonePreferences(t.Context(), func(p *settings.ClonePreferences) error {
		p.GloballyEnabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "GIT HOOKS MISSING") {
		t.Fatalf("missing clone warning, got: %q", got)
	}
	if !strings.Contains(got, "Marker cleared") {
		t.Errorf("warning must say the marker was cleared, got: %q", got)
	}

	// The globally_enabled marker is a run-once latch that the lazy setup never
	// revisits, so doctor must have cleared it...
	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if prefs.GloballyEnabled {
		t.Fatal("doctor must clear the globally_enabled marker when git hooks are missing")
	}

	// ...which lets the next hook activity actually reconverge.
	settings.ClearGlobalModeCache()
	t.Cleanup(settings.ClearGlobalModeCache)
	strategy.MaybeEnsureGlobalSetup(t.Context())
	if !strategy.IsGitHookInstalled(t.Context()) {
		t.Error("MaybeEnsureGlobalSetup after the marker clear must reinstall the git hooks")
	}
}

func TestCheckGlobalTracking_WarnsOnMalformedUserSettings(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":`) // truncated JSON

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "USER SETTINGS UNREADABLE") {
		t.Fatalf("missing malformed-settings warning, got: %q", got)
	}
	if !strings.Contains(got, settings.UserSettingsPath()) {
		t.Errorf("warning must name the settings file, got: %q", got)
	}
	if !strings.Contains(got, "machine-wide") {
		t.Errorf("warning must state the machine-wide consequence, got: %q", got)
	}
}

func TestCheckGlobalTracking_WarnsOnUnusableExcludePatterns(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg,
		`{"global":{"enabled":true,"exclude_paths":["relative/path","~bob/code/**","/srv/["],"exclude_origins":["github.com/acme/["]}}`)

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "UNUSABLE EXCLUDE PATTERNS") {
		t.Fatalf("missing pattern warning, got: %q", got)
	}
	for _, want := range []string{
		"exclude_paths[0]", "exclude_paths[1]", "exclude_paths[2]",
		"exclude_origins[0]", settings.UserSettingsPath(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning missing %q, got: %q", want, got)
		}
	}
}

func TestCheckGlobalTracking_InfoOnUnnormalizableOrigin(t *testing.T) {
	setupTestRepo(t)
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	testutil.AddRemote(t, repoDir, "origin", "/srv/git/widgets.git")
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_origins":["github.com/acme/*"]}}`)

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "origin not checkable in this repo (informational)") {
		t.Fatalf("missing informational origin note, got: %q", got)
	}
	if !strings.Contains(got, "/srv/git/widgets.git") {
		t.Errorf("note must name the unnormalizable origin URL, got: %q", got)
	}
}

func TestCheckGlobalTracking_ValidationSilentWhenClean(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg,
		`{"global":{"enabled":true,"exclude_paths":["~/scratch/**","/srv/tmp/**"],"exclude_origins":["github.com/acme/*"]}}`)

	got := runCheckGlobalTracking(t)
	for _, banned := range []string{"USER SETTINGS UNREADABLE", "UNUSABLE EXCLUDE PATTERNS", "origin not checkable"} {
		if strings.Contains(got, banned) {
			t.Errorf("clean config must not trigger %q, got: %q", banned, got)
		}
	}
}

func TestCheckGlobalTracking_ValidationSilentWhenTierOff(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	// Bad patterns, but the tier is off: nothing gates on them, stay silent.
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":false,"exclude_paths":["relative/path"]}}`)

	if got := runCheckGlobalTracking(t); got != "" {
		t.Errorf("disabled tier must be silent even with bad patterns, got: %q", got)
	}
}
