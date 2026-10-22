package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// loadPIIEnabled returns redaction.pii.enabled and whether a pii block exists.
func loadPIIEnabled(t *testing.T) (enabled, present bool) {
	t.Helper()
	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.Redaction == nil || s.Redaction.PII == nil {
		return false, false
	}
	return s.Redaction.PII.Enabled, true
}

func TestConfigureCmd_RedactPII_PersistsAndKeepsCategories(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "redaction": {"pii": {"enabled": false, "address": true}}}`)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--redact-pii"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --redact-pii error = %v", err)
	}

	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.Redaction == nil || s.Redaction.PII == nil || !s.Redaction.PII.Enabled {
		t.Fatalf("redaction.pii = %+v, want enabled", s.Redaction)
	}
	if s.Redaction.PII.Address == nil || !*s.Redaction.PII.Address {
		t.Error("existing pii.address setting was lost")
	}
	if !strings.Contains(stdout.String(), "PII redaction on") {
		t.Errorf("stdout = %q, want a confirmation", stdout.String())
	}
	if strings.Contains(stdout.String(), "Reinstalled git hook") {
		t.Errorf("--redact-pii alone reinstalled the git hook: %s", stdout.String())
	}
}

func TestConfigureCmd_RedactPIIFalse_TurnsItOff(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "redaction": {"pii": {"enabled": true}}}`)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--redact-pii=false"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --redact-pii=false error = %v", err)
	}
	if enabled, present := loadPIIEnabled(t); !present || enabled {
		t.Errorf("pii enabled=%v present=%v, want present and disabled", enabled, present)
	}
}

// On a configured repo, --redact-pii must be written without opening the
// agent picker, which owns no settings writes.
func TestEnableCmd_ConfiguredRepo_RedactPII(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--redact-pii"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable --redact-pii error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if enabled, _ := loadPIIEnabled(t); !enabled {
		t.Error("enable --redact-pii on a configured repo did not enable PII redaction")
	}
	if out := stdout.String() + stderr.String(); strings.Contains(out, "Cannot show agent selection") {
		t.Errorf("enable --redact-pii opened agent management: %s", out)
	}
}

func TestEnableCmd_AgentRedactPII(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	testutil.WriteFile(t, ".", "f.txt", "init")
	testutil.GitAdd(t, ".", "f.txt")
	testutil.GitCommit(t, ".", "init")

	cmd := newEnableCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--agent", "claude-code", "--telemetry=false", "--redact-pii"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable --agent --redact-pii error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if enabled, _ := loadPIIEnabled(t); !enabled {
		t.Error("enable --agent --redact-pii did not enable PII redaction")
	}
}

func TestEnableCmd_YesLeavesPIIUnsetUnlessFlagged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extra       []string
		wantPresent bool
	}{
		{name: "yes alone", extra: nil, wantPresent: false},
		{name: "yes with flag", extra: []string{"--redact-pii"}, wantPresent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Cannot use t.Parallel() because we use t.Chdir
			setupTestRepo(t)
			testutil.WriteFile(t, ".", "f.txt", "init")
			testutil.GitAdd(t, ".", "f.txt")
			testutil.GitCommit(t, ".", "init")

			cmd := newEnableCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"--yes", "--agent", "claude-code", "--telemetry=false"}, tc.extra...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("enable error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			enabled, present := loadPIIEnabled(t)
			if present != tc.wantPresent || enabled != tc.wantPresent {
				t.Errorf("pii enabled=%v present=%v, want both %v", enabled, present, tc.wantPresent)
			}
		})
	}
}

// stubRedactPIIPrompt makes the first-run prompt reachable without a terminal
// and records whether it was shown. Not parallel-safe: swaps package seams.
func stubRedactPIIPrompt(t *testing.T, canPrompt, answer bool) *bool {
	t.Helper()
	prevCan, prevPrompt := canPromptRedactPII, promptRedactPII
	asked := false
	canPromptRedactPII = func() bool { return canPrompt }
	promptRedactPII = func() (bool, error) {
		asked = true
		return answer, nil
	}
	t.Cleanup(func() { canPromptRedactPII, promptRedactPII = prevCan, prevPrompt })
	return &asked
}

// Not parallel: swaps package-level prompt seams.
func TestChooseRedactPII(t *testing.T) {
	configured := func() *EntireSettings {
		s := &EntireSettings{}
		applyRedactPII(s, false)
		return s
	}
	tests := []struct {
		name        string
		settings    *EntireSettings
		opts        EnableOptions
		canPrompt   bool
		answer      bool
		wantAsked   bool
		wantPresent bool
		wantEnabled bool
	}{
		{name: "yes answer enables", settings: &EntireSettings{}, canPrompt: true, answer: true, wantAsked: true, wantPresent: true, wantEnabled: true},
		{name: "no answer writes nothing", settings: &EntireSettings{}, canPrompt: true, answer: false, wantAsked: true},
		{name: "no terminal does not ask", settings: &EntireSettings{}, canPrompt: false},
		{name: "--yes does not ask", settings: &EntireSettings{}, opts: EnableOptions{Yes: true}, canPrompt: true},
		{name: "already configured does not ask", settings: configured(), canPrompt: true, answer: true, wantPresent: true},
		{name: "flag wins without asking", settings: &EntireSettings{}, opts: EnableOptions{RedactPII: true, RedactPIISet: true}, canPrompt: true, wantPresent: true, wantEnabled: true},
		{name: "false flag records off", settings: &EntireSettings{}, opts: EnableOptions{RedactPIISet: true}, canPrompt: true, wantPresent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := stubRedactPIIPrompt(t, tt.canPrompt, tt.answer)
			if err := chooseRedactPII(tt.settings, tt.opts); err != nil {
				t.Fatalf("chooseRedactPII: %v", err)
			}
			if *asked != tt.wantAsked {
				t.Errorf("asked = %v, want %v", *asked, tt.wantAsked)
			}
			present := tt.settings.Redaction != nil && tt.settings.Redaction.PII != nil
			if present != tt.wantPresent {
				t.Fatalf("pii present = %v, want %v", present, tt.wantPresent)
			}
			if present && tt.settings.Redaction.PII.Enabled != tt.wantEnabled {
				t.Errorf("pii enabled = %v, want %v", tt.settings.Redaction.PII.Enabled, tt.wantEnabled)
			}
		})
	}
}

// A local file written by --redact-pii must hold only the PII key, so it
// cannot override the project file's "enabled": false and resume capture.
func TestConfigureCmd_LocalRedactPII_DoesNotReenable(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, `{"enabled": false}`)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--local", "--redact-pii"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --local --redact-pii error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(".entire", "settings.local.json"))
	if err != nil {
		t.Fatalf("read local settings: %v", err)
	}
	if got, want := strings.Join(strings.Fields(string(data)), ""), `{"redaction":{"pii":{"enabled":true}}}`; got != want {
		t.Errorf("local settings = %s, want only the pii key %s", got, want)
	}
	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.Enabled {
		t.Error("configure --local --redact-pii re-enabled a disabled repo")
	}
}
