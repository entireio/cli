package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"

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
	if !strings.Contains(got, "self-heals") {
		t.Errorf("warning must say the lazy path self-heals, got: %q", got)
	}
}
