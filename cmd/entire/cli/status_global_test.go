package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
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

// installClaudeUserHooksForTest runs the real user-level installer against
// the isolated home, so the fixture always satisfies the completeness
// predicate AreUserHooksInstalled enforces (Stop plus the tool-use matchers)
// and marks claude-code as covered.
func installClaudeUserHooksForTest(t *testing.T, home string) {
	t.Helper()
	if _, err := (&claudecode.ClaudeCodeAgent{}).InstallUserHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("fixture did not land in the isolated home: %v", err)
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

// TestRunStatus_GlobalTrackingLine_ExcludedRepo: inside a repo the tier
// carves out, "on (N agents covered)" reads as covered when no session here
// is tracked — the line must name the carve-out instead.
func TestRunStatus_GlobalTrackingLine_ExcludedRepo(t *testing.T) {
	setupTestRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	home := isolateUserHome(t)
	excludeJSON, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_paths":[`+string(excludeJSON)+`]}}`)
	installClaudeUserHooksForTest(t, home)
	settings.ClearGlobalModeCache()
	t.Cleanup(settings.ClearGlobalModeCache)

	var out bytes.Buffer
	if err := runStatus(context.Background(), &out, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "global tracking: on (this repo is excluded)") {
		t.Errorf("missing excluded-repo line, got: %s", out.String())
	}
	if strings.Contains(out.String(), "covered)") {
		t.Errorf("excluded repo must not read as covered, got: %s", out.String())
	}
}

// TestRunStatus_GlobalTrackingLine_RepoDisabled: a repo-level veto
// (settings.json enabled=false) carves this repo out of the tier exactly
// like an exclusion — the line must say so instead of reading as covered.
func TestRunStatus_GlobalTrackingLine_RepoDisabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	home := isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	installClaudeUserHooksForTest(t, home)
	settings.ClearGlobalModeCache()
	t.Cleanup(settings.ClearGlobalModeCache)

	var out bytes.Buffer
	if err := runStatus(context.Background(), &out, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "global tracking: on (inactive here: repo-level setup has Entire disabled)") {
		t.Errorf("missing repo-disabled carve-out, got: %s", out.String())
	}
	if strings.Contains(out.String(), "covered)") {
		t.Errorf("repo-disabled repo must not read as covered, got: %s", out.String())
	}
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

// setupGloballyEnrolledStatusRepo enters a fresh globally-enrolled repo (no
// repo-level setup). Not parallel-safe: t.Chdir/t.Setenv.
func setupGloballyEnrolledStatusRepo(t *testing.T, userSettings string) {
	t.Helper()
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	isolateUserHome(t)
	writeGlobalUserSettings(t, cfg, userSettings)
	settings.ClearGlobalModeCache()
	t.Cleanup(settings.ClearGlobalModeCache)
}

// TestRunStatus_GlobalTrust covers the human trust line and the JSON keys,
// pinned by raw string (unmarshal is case-insensitive across a key rename).
func TestRunStatus_GlobalTrust(t *testing.T) {
	for _, tc := range []struct {
		name         string
		userSettings string
		trustRepo    bool
		repoSetup    bool
		wantText     string // "" = neither trust line renders
		wantJSON     []string
	}{
		{"untrusted enrolled repo shows sync held", `{"global":{"enabled":true}}`, false, false,
			"sync held — repo not trusted · run `entire trust`",
			[]string{`"trust_state":"untrusted"`, `"trust_source":"none"`}},
		{"trusted repo names the per-repo source", `{"global":{"enabled":true}}`, true, false,
			"trusted (this repo)",
			[]string{`"trust_state":"trusted"`, `"trust_source":"repo"`}},
		{"trust_all names trust_all", `{"global":{"enabled":true,"trust_all":true}}`, false, false,
			"trusted (trust_all)", []string{`"trust_source":"trust_all"`}},
		{"repo-level setup is not_applicable and silent", `{"global":{"enabled":true}}`, false, true,
			"", []string{`"trust_state":"not_applicable"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupGloballyEnrolledStatusRepo(t, tc.userSettings)
			if tc.trustRepo {
				if _, err := settings.TrustCurrentRepo(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.repoSetup {
				writeSettings(t, testSettingsEnabled)
			}

			var text bytes.Buffer
			if err := runStatus(context.Background(), &text, false, false); err != nil {
				t.Fatal(err)
			}
			if tc.wantText != "" && !strings.Contains(text.String(), tc.wantText) {
				t.Errorf("status text missing %q, got: %s", tc.wantText, text.String())
			}
			if tc.wantText == "" && (strings.Contains(text.String(), "sync held") || strings.Contains(text.String(), "trusted (")) {
				t.Errorf("not_applicable state must render no trust line, got: %s", text.String())
			}

			var jsonOut bytes.Buffer
			if err := runStatus(context.Background(), &jsonOut, false, true); err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(jsonOut.Bytes(), &m); err != nil {
				t.Fatalf("parse status JSON: %v (%s)", err, jsonOut.String())
			}
			gt := string(m["global_tracking"])
			for _, want := range tc.wantJSON {
				if !strings.Contains(gt, want) {
					t.Errorf("global_tracking missing %s, got: %s", want, gt)
				}
			}
		})
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

	// The enabled in-repo shape pins the exact JSON keys: Go's unmarshal is
	// case-insensitive, so struct-decode assertions would survive a key
	// rename that breaks every external consumer.
	t.Run("enabled shape has exact keys, excluded repo carries the reason", func(t *testing.T) {
		setupTestRepo(t)
		root, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		home := isolateUserHome(t)
		excludeJSON, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true,"exclude_paths":[`+string(excludeJSON)+`]}}`)
		installClaudeUserHooksForTest(t, home)
		settings.ClearGlobalModeCache()
		t.Cleanup(settings.ClearGlobalModeCache)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatal(err)
		}
		m := decode(t, &out)
		var gt map[string]json.RawMessage
		if err := json.Unmarshal(m["global_tracking"], &gt); err != nil {
			t.Fatalf("global_tracking missing: %s", out.String())
		}
		for key, want := range map[string]string{
			"enabled":         "true",
			"agents_covered":  "1",
			"active_here":     "false",
			"inactive_reason": `"repo_excluded"`,
		} {
			if got, ok := gt[key]; !ok || string(got) != want {
				t.Errorf("global_tracking[%q] = %s (present=%v), want %s", key, got, ok, want)
			}
		}
	})

	t.Run("repo-level disable carries inactive_reason repo_disabled", func(t *testing.T) {
		setupTestRepo(t)
		writeSettings(t, testSettingsDisabled)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		isolateUserHome(t)
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		settings.ClearGlobalModeCache()
		t.Cleanup(settings.ClearGlobalModeCache)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatal(err)
		}
		m := decode(t, &out)
		var gt map[string]json.RawMessage
		if err := json.Unmarshal(m["global_tracking"], &gt); err != nil {
			t.Fatalf("global_tracking missing: %s", out.String())
		}
		var activeHere bool
		if err := json.Unmarshal(gt["active_here"], &activeHere); err != nil || activeHere {
			t.Errorf("active_here = %s, want false", gt["active_here"])
		}
		if got := string(gt["inactive_reason"]); got != `"repo_disabled"` {
			t.Errorf("inactive_reason = %s, want \"repo_disabled\"", got)
		}
	})

	t.Run("outside a repo omits the per-repo keys", func(t *testing.T) {
		setupTestDir(t)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		isolateUserHome(t)
		writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatal(err)
		}
		m := decode(t, &out)
		var gt map[string]json.RawMessage
		if err := json.Unmarshal(m["global_tracking"], &gt); err != nil {
			t.Fatalf("global_tracking missing: %s", out.String())
		}
		for _, key := range []string{"active_here", "inactive_reason", "trust_state", "trust_source"} {
			if _, ok := gt[key]; ok {
				t.Errorf("%q must be omitted outside a repository: %s", key, m["global_tracking"])
			}
		}
	})
}
