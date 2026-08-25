package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
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
	checkGlobalTracking(cmd, false)
	return out.String()
}

func isolateUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func installUserAgentHooks(ctx context.Context, t *testing.T, _ io.Writer) {
	t.Helper()
	supports, _ := agent.UserHookSupports()
	for _, ua := range supports {
		if _, err := ua.Support.InstallUserHooks(ctx); err != nil {
			t.Fatalf("install %s user hooks: %v", ua.Name, err)
		}
	}
}

// TestCheckGlobalTracking_SettingsShapes drives the pure settings→output rows:
// silence while off/unconfigured/clean, and each validation warning.
func TestCheckGlobalTracking_SettingsShapes(t *testing.T) {
	cases := []struct {
		name         string
		userSettings string // "" = no file
		want, banned []string
	}{
		{"unconfigured tier is silent", "", nil,
			[]string{"Global tracking"}},
		{"off tier is silent", `{"global":{"enabled":false}}`, nil,
			[]string{"Global tracking"}},
		{"off tier is silent even with bad patterns", `{"global":{"enabled":false,"exclude_paths":["relative/path"]}}`, nil,
			[]string{"UNUSABLE SETTINGS ENTRIES"}},
		{"enabled without user hooks warns", `{"global":{"enabled":true}}`,
			[]string{"USER-LEVEL AGENT HOOKS MISSING", "claude-code", "gemini", "affected agent settings"}, nil},
		{"malformed settings warn machine-wide", `{"global":`,
			[]string{"USER SETTINGS UNREADABLE", "<settings-path>", "machine-wide"}, nil},
		{"unusable exclude patterns are enumerated",
			`{"global":{"enabled":true,"exclude_paths":["relative/path","~bob/code/**","/srv/["],"exclude_origins":["github.com/acme/["],"trusted_paths":["~bob/code/repo"]}}`,
			// trusted_paths joins the same section: the gate skips an unusable
			// entry, so doctor must name it too.
			[]string{"UNUSABLE SETTINGS ENTRIES", "exclude_paths[0]", "exclude_paths[1]", "exclude_paths[2]", "exclude_origins[0]", "trusted_paths[0]", "<settings-path>"}, nil},
		{"clean config triggers no validation warnings",
			`{"global":{"enabled":true,"exclude_paths":["~/scratch/**","/srv/tmp/**"],"exclude_origins":["github.com/acme/*"]}}`,
			nil, []string{"USER SETTINGS UNREADABLE", "UNUSABLE SETTINGS ENTRIES", "origin not checkable"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupTestRepo(t)
			cfg := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", cfg)
			isolateUserHome(t)
			if c.userSettings != "" {
				writeGlobalUserSettings(t, cfg, c.userSettings)
			}
			got := runCheckGlobalTracking(t)
			for _, w := range c.want {
				// Resolved here: UserSettingsPath depends on the subtest's env.
				w = strings.ReplaceAll(w, "<settings-path>", settings.UserSettingsPath())
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q, got: %q", w, got)
				}
			}
			for _, b := range c.banned {
				if strings.Contains(got, b) {
					t.Errorf("output must not contain %q, got: %q", b, got)
				}
			}
		})
	}
}

func TestCheckGlobalTracking_OKWhenUserHooksInstalled(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	var buf bytes.Buffer
	installUserAgentHooks(t.Context(), t, &buf)

	if got := runCheckGlobalTracking(t); !strings.Contains(got, "✓ Global tracking: user-level agent hooks OK") {
		t.Fatalf("expected OK line, got: %q", got)
	}
}

func TestCheckGlobalTracking_ForceRepairsMissingUserHooks(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	checkGlobalTracking(cmd, true)

	for _, ua := range func() []agent.UserHookAgent {
		supported, _ := agent.UserHookSupports()
		return supported
	}() {
		installed, err := ua.Support.AreUserHooksInstalled(t.Context())
		if err != nil || !installed {
			t.Fatalf("%s user hooks after doctor --force = %v, %v", ua.Name, installed, err)
		}
	}
}

func TestCheckGlobalTracking_WarnsOnMarkedCloneWithoutGitHooks(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	var buf bytes.Buffer
	installUserAgentHooks(t.Context(), t, &buf)

	// Mark this clone as lazily enabled without installing its git hooks.
	repository, err := repopolicy.ResolveRepository(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := repopolicy.WriteSetupRecord(repository, repopolicy.SetupRecord{GitHooksSpec: 1, PrimaryRefSpec: 1}); err != nil {
		t.Fatal(err)
	}

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "GIT HOOKS MISSING") || !strings.Contains(got, "marked stale") {
		t.Fatalf("missing drift warning with marker note, got: %q", got)
	}
	// The marker is a run-once latch the lazy setup never revisits, so doctor
	// must have cleared it, letting the next hook activity reconverge.
	record, _, err := repopolicy.ReadSetupRecord(repository)
	if err != nil {
		t.Fatal(err)
	}
	if record.GitHooksSpec != 0 {
		t.Fatal("doctor must mark the git-hooks component stale when git hooks are missing")
	}
	settings.ClearGlobalModeCache()
	t.Cleanup(settings.ClearGlobalModeCache)
	strategy.MaybeEnsureGlobalSetup(t.Context())
	if !strategy.IsGitHookInstalled(t.Context()) {
		t.Error("MaybeEnsureGlobalSetup after the marker clear must reinstall the git hooks")
	}
}

// A worktree-resident core.hooksPath means the lazy setup deliberately skipped
// installation: doctor explains instead of reporting drift, and must NOT clear
// the marker (clearing re-runs a setup that skips again).
func TestCheckGlobalTracking_ExplainsWorktreeResidentHooksPath(t *testing.T) {
	setupTestRepo(t)
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	gitCfg := exec.CommandContext(t.Context(), "git", "config", "core.hooksPath", ".husky")
	gitCfg.Dir = repoDir
	if out, cfgErr := gitCfg.CombinedOutput(); cfgErr != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", cfgErr, out)
	}
	strategy.ClearHooksDirCache()
	t.Cleanup(strategy.ClearHooksDirCache)

	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	var buf bytes.Buffer
	installUserAgentHooks(t.Context(), t, &buf)
	repository, err := repopolicy.ResolveRepository(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := repopolicy.WriteSetupRecord(repository, repopolicy.SetupRecord{GitHooksSpec: 1, PrimaryRefSpec: 1}); err != nil {
		t.Fatal(err)
	}

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "GIT HOOKS SKIPPED (core.hooksPath inside the worktree)") {
		t.Fatalf("missing hooksPath explanation, got: %q", got)
	}
	for _, banned := range []string{"GIT HOOKS MISSING", "Marker cleared"} {
		if strings.Contains(got, banned) {
			t.Errorf("hooksPath shape must not be treated as drift (%q), got: %q", banned, got)
		}
	}
	record, _, err := repopolicy.ReadSetupRecord(repository)
	if err != nil {
		t.Fatal(err)
	}
	if record.GitHooksSpec != 1 {
		t.Error("doctor must NOT stale the setup record for a worktree-resident hooksPath")
	}
}

// When the hooksPath residency probe itself fails, doctor prints UNVERIFIED
// and mutates nothing — the lazy setup treats the same error as "skip", so
// clearing the marker would promise a reinstall that never happens.
func TestCheckGlobalTracking_ProbeErrorReportsUnverifiedWithoutClearing(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	var buf bytes.Buffer
	installUserAgentHooks(t.Context(), t, &buf)
	repository, err := repopolicy.ResolveRepository(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := repopolicy.WriteSetupRecord(repository, repopolicy.SetupRecord{GitHooksSpec: 1, PrimaryRefSpec: 1}); err != nil {
		t.Fatal(err)
	}

	strategy.SetHooksDirProbeErrorForTesting(errors.New("forced probe failure (test seam)"))
	t.Cleanup(func() { strategy.SetHooksDirProbeErrorForTesting(nil) })

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "GIT HOOK STATE UNVERIFIED") || !strings.Contains(got, "forced probe failure (test seam)") {
		t.Fatalf("probe error must report UNVERIFIED with the cause, got: %q", got)
	}
	for _, banned := range []string{"GIT HOOKS MISSING", "Marker cleared", "GIT HOOKS SKIPPED"} {
		if strings.Contains(got, banned) {
			t.Errorf("probe-error shape must not be treated as drift (%q), got: %q", banned, got)
		}
	}
	record, _, err := repopolicy.ReadSetupRecord(repository)
	if err != nil {
		t.Fatal(err)
	}
	if record.GitHooksSpec != 1 {
		t.Error("doctor must NOT stale the setup record when the residency probe fails")
	}
}

// An unreadable agent config is "unverifiable", not "missing" (its remedy
// differs) and never yields the OK line; with unusable exclude patterns in the
// same run, every section appears in the function's stable order.
func TestCheckGlobalTracking_UnverifiableAgentConfigAndSectionOrder(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	home := isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_paths":["relative/path"]}}`)

	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := runCheckGlobalTracking(t)
	if strings.Contains(got, "user-level agent hooks OK") {
		t.Errorf("OK line must not appear alongside problems, got: %q", got)
	}
	idxMissing := strings.Index(got, "USER-LEVEL AGENT HOOKS MISSING")
	idxUnverifiable := strings.Index(got, "USER-LEVEL AGENT HOOKS UNVERIFIABLE")
	idxPatterns := strings.Index(got, "UNUSABLE SETTINGS ENTRIES")
	if idxMissing < 0 || idxUnverifiable < 0 || idxPatterns < 0 {
		t.Fatalf("all three sections must be reported (missing=%d unverifiable=%d patterns=%d), got: %q",
			idxMissing, idxUnverifiable, idxPatterns, got)
	}
	if !strings.Contains(got[idxUnverifiable:], "claude-code") || !strings.Contains(got[idxMissing:idxUnverifiable], "gemini") {
		t.Errorf("claude-code must be unverifiable and gemini missing, got: %q", got)
	}
	if idxMissing >= idxUnverifiable || idxUnverifiable >= idxPatterns {
		t.Errorf("sections out of stable order (missing=%d unverifiable=%d patterns=%d)", idxMissing, idxUnverifiable, idxPatterns)
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
	if !strings.Contains(got, "origin not checkable in this repo (informational)") || !strings.Contains(got, "/srv/git/widgets.git") {
		t.Fatalf("missing informational origin note naming the URL, got: %q", got)
	}
}

// The hold is a consent state, not a failure — doctor explains it at INFO level.
func TestCheckGlobalTracking_InfoOnUntrustedEnrolledRepo(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	settings.ClearGlobalModeCache()
	t.Cleanup(settings.ClearGlobalModeCache)

	got := runCheckGlobalTracking(t)
	if !strings.Contains(got, "checkpoint sync held in this repo (informational)") {
		t.Fatalf("missing informational hold note, got: %q", got)
	}
	if !strings.Contains(got, "run `entire trust` to sync") {
		t.Errorf("hold note missing the remedy, got: %q", got)
	}
}
