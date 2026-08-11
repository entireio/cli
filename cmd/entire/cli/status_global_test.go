package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// No t.Parallel in this file: every test uses t.Chdir and/or t.Setenv.

// writeGlobalUserSettings persists a user-global settings payload into the
// isolated ENTIRE_CONFIG_DIR.
func writeGlobalUserSettings(t *testing.T, cfg, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// installClaudeUserHooksForTest writes a minimal ~/.claude/settings.json whose
// Stop hook is recognizably Entire's, marking claude-code as covered.
func installClaudeUserHooksForTest(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	content := `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"entire hooks claude-code stop"}]}]}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunStatus_GlobalTrackingLine(t *testing.T) {
	t.Run("omitted while unconfigured", func(t *testing.T) {
		setupTestRepo(t)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		isolateUserHome(t)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "global tracking") {
			t.Errorf("unconfigured tier must be omitted, got: %s", out.String())
		}
	})

	t.Run("on with coverage count in a not-set-up repo", func(t *testing.T) {
		setupTestRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		home := isolateUserHome(t)
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		installClaudeUserHooksForTest(t, home)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "global tracking: on (1 agent covered)") {
			t.Errorf("missing global tracking line, got: %s", out.String())
		}
	})

	t.Run("off", func(t *testing.T) {
		setupTestRepo(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		isolateUserHome(t)
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":false}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "global tracking: off") {
			t.Errorf("missing off line, got: %s", out.String())
		}
	})

	t.Run("shown alongside repo-level status when set up", func(t *testing.T) {
		setupTestRepo(t)
		writeSettings(t, testSettingsEnabled)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		isolateUserHome(t)
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Enabled") || !strings.Contains(out.String(), "global tracking: on (0 agents covered)") {
			t.Errorf("missing repo status or global line, got: %s", out.String())
		}
	})
}

// TestRunStatus_OutsideGitRepo_GlobalLine: status must work outside a git
// repository and report the machine-wide tier there.
func TestRunStatus_OutsideGitRepo_GlobalLine(t *testing.T) {
	setupTestDir(t) // no git init
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	home := isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	installClaudeUserHooksForTest(t, home)

	var out bytes.Buffer
	if err := runStatus(context.Background(), &out, false, false); err != nil {
		t.Fatalf("status must not fail outside a git repo: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "✕ not a git repository") {
		t.Errorf("missing not-a-git-repo note, got: %s", got)
	}
	if !strings.Contains(got, "global tracking: on (1 agent covered)") {
		t.Errorf("missing global tracking line outside repo, got: %s", got)
	}
}

func TestRunStatusJSON_GlobalTracking(t *testing.T) {
	decode := func(t *testing.T, out *bytes.Buffer) map[string]json.RawMessage {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("parse status JSON: %v (%s)", err, out.String())
		}
		return m
	}

	t.Run("outside a git repo includes the tier", func(t *testing.T) {
		setupTestDir(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		home := isolateUserHome(t)
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		installClaudeUserHooksForTest(t, home)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatal(err)
		}
		m := decode(t, &out)
		var gt struct {
			Enabled       bool `json:"enabled"`
			AgentsCovered int  `json:"agents_covered"`
		}
		if err := json.Unmarshal(m["global_tracking"], &gt); err != nil {
			t.Fatalf("global_tracking missing: %s", out.String())
		}
		if !gt.Enabled || gt.AgentsCovered != 1 {
			t.Errorf("global_tracking = %+v, want enabled with 1 agent covered", gt)
		}
	})

	t.Run("off tier serializes enabled=false", func(t *testing.T) {
		setupTestRepo(t)
		writeSettings(t, testSettingsEnabled)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		isolateUserHome(t)
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":false}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatal(err)
		}
		m := decode(t, &out)
		if !strings.Contains(string(m["global_tracking"]), `"enabled":false`) {
			t.Errorf("global_tracking = %s, want enabled:false", m["global_tracking"])
		}
	})

	t.Run("unconfigured tier is omitted", func(t *testing.T) {
		setupTestRepo(t)
		writeSettings(t, testSettingsEnabled)
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		isolateUserHome(t)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatal(err)
		}
		m := decode(t, &out)
		if _, ok := m["global_tracking"]; ok {
			t.Errorf("global_tracking must be omitted while unconfigured: %s", out.String())
		}
	})
}
