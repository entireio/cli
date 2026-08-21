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
// the isolated home, so the fixture satisfies the completeness predicate and
// marks claude-code as covered.
func installClaudeUserHooksForTest(t *testing.T, home string) {
	t.Helper()
	if _, err := (&claudecode.ClaudeCodeAgent{}).InstallUserHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("fixture did not land in the isolated home: %v", err)
	}
}

// globalStatusFixture is the shared scaffold for the status-surface rows.
type globalStatusFixture struct {
	gitRepo      bool
	repoSettings string // "" = no repo-level settings file
	userSettings string // "" = no user settings file
	installHooks bool
	excludeSelf  bool // exclude this worktree root via exclude_paths
}

func setUpGlobalStatus(t *testing.T, f globalStatusFixture) {
	t.Helper()
	if f.gitRepo {
		setupTestRepo(t)
	} else {
		setupTestDir(t)
	}
	if f.repoSettings != "" {
		writeSettings(t, f.repoSettings)
	}
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	home := isolateUserHome(t)
	userSettings := f.userSettings
	if f.excludeSelf {
		root, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		excludeJSON, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		userSettings = `{"global":{"enabled":true,"exclude_paths":[` + string(excludeJSON) + `]}}`
	}
	if userSettings != "" {
		writeGlobalUserSettings(t, cfg, userSettings)
	}
	if f.installHooks {
		installClaudeUserHooksForTest(t, home)
	}
	settings.ClearGlobalModeCache()
	t.Cleanup(settings.ClearGlobalModeCache)
}

func TestRunStatus_GlobalTrackingLine(t *testing.T) {
	cases := []struct {
		name         string
		fixture      globalStatusFixture
		want, banned []string
	}{
		{"omitted while unconfigured",
			globalStatusFixture{gitRepo: true},
			nil, []string{"global tracking"}},
		{"on with coverage count in a not-set-up repo",
			globalStatusFixture{gitRepo: true, userSettings: `{"global":{"enabled":true}}`, installHooks: true},
			[]string{"global tracking: on (1 agent covered)"}, nil},
		{"off",
			globalStatusFixture{gitRepo: true, userSettings: `{"global":{"enabled":false}}`},
			[]string{"global tracking: off"}, nil},
		{"shown alongside repo-level status when set up",
			globalStatusFixture{gitRepo: true, repoSettings: testSettingsEnabled, userSettings: `{"global":{"enabled":true}}`},
			[]string{"Enabled", "global tracking: on (0 agents covered)"}, nil},
		// Carve-outs must not read as covered.
		{"excluded repo names the carve-out",
			globalStatusFixture{gitRepo: true, excludeSelf: true, installHooks: true},
			[]string{"global tracking: on (this repo is excluded)"}, []string{"covered)"}},
		{"repo-level disable names the veto",
			globalStatusFixture{gitRepo: true, repoSettings: testSettingsDisabled, userSettings: `{"global":{"enabled":true}}`, installHooks: true},
			[]string{"global tracking: on (inactive here: repo-level setup has Entire disabled)"}, []string{"covered)"}},
		// Status must work outside a git repository and report the tier there.
		{"outside a git repo",
			globalStatusFixture{userSettings: `{"global":{"enabled":true}}`, installHooks: true},
			[]string{"✕ not a git repository", "global tracking: on (1 agent covered)"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setUpGlobalStatus(t, c.fixture)
			var out bytes.Buffer
			if err := runStatus(context.Background(), &out, false, false); err != nil {
				t.Fatal(err)
			}
			for _, w := range c.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output missing %q, got: %s", w, out.String())
				}
			}
			for _, b := range c.banned {
				if strings.Contains(out.String(), b) {
					t.Errorf("output must not contain %q, got: %s", b, out.String())
				}
			}
		})
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
		repoSettings string
		wantText     string // "" = neither trust line renders
		wantJSON     []string
	}{
		{"untrusted enrolled repo shows sync held", `{"global":{"enabled":true}}`, false, "",
			"sync held — repo not trusted · run `entire trust`",
			[]string{`"trust_state":"untrusted"`, `"trust_source":"none"`}},
		{"trusted repo names the per-repo source", `{"global":{"enabled":true}}`, true, "",
			"trusted (this repo)",
			[]string{`"trust_state":"trusted"`, `"trust_source":"repo"`}},
		{"trust_all names trust_all", `{"global":{"enabled":true,"trust_all":true}}`, false, "",
			"trusted (trust_all)", []string{`"trust_source":"trust_all"`}},
		{"incidental repo settings retain global trust", `{"global":{"enabled":true,"trust_all":true}}`, false, `{"investigate":{"max_turns":4}}`,
			"trusted (trust_all)", []string{`"trust_state":"trusted"`, `"trust_source":"trust_all"`}},
		{"repo-level setup is not_applicable and silent", `{"global":{"enabled":true}}`, false, `{"enabled":true}`,
			"", []string{`"trust_state":"not_applicable"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupGloballyEnrolledStatusRepo(t, tc.userSettings)
			if tc.trustRepo {
				if _, err := settings.TrustCurrentRepo(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.repoSettings != "" {
				writeSettings(t, tc.repoSettings)
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
	decodeGT := func(t *testing.T, out *bytes.Buffer) (map[string]json.RawMessage, bool) {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("parse status JSON: %v (%s)", err, out.String())
		}
		raw, ok := m["global_tracking"]
		if !ok {
			return nil, false
		}
		var gt map[string]json.RawMessage
		if err := json.Unmarshal(raw, &gt); err != nil {
			t.Fatalf("parse global_tracking: %v (%s)", err, raw)
		}
		return gt, true
	}

	// Raw-key assertions on purpose: Go's unmarshal is case-insensitive, so
	// struct-decode assertions would survive a key rename that breaks every
	// external consumer.
	cases := []struct {
		name    string
		fixture globalStatusFixture
		present bool
		keys    map[string]string // exact raw JSON per key
		absent  []string
	}{
		{"unconfigured tier is omitted",
			globalStatusFixture{gitRepo: true, repoSettings: testSettingsEnabled},
			false, nil, nil},
		{"off tier serializes enabled=false",
			globalStatusFixture{gitRepo: true, repoSettings: testSettingsEnabled, userSettings: `{"global":{"enabled":false}}`},
			true, map[string]string{"enabled": "false"}, nil},
		{"excluded repo has exact keys and the reason",
			globalStatusFixture{gitRepo: true, excludeSelf: true, installHooks: true},
			true, map[string]string{
				"enabled": "true", "agents_covered": "1",
				"active_here": "false", "inactive_reason": `"repo_excluded"`,
			}, nil},
		{"repo-level disable carries inactive_reason repo_disabled",
			globalStatusFixture{gitRepo: true, repoSettings: testSettingsDisabled, userSettings: `{"global":{"enabled":true}}`},
			true, map[string]string{"active_here": "false", "inactive_reason": `"repo_disabled"`}, nil},
		{"outside a repo includes the tier, omits the per-repo keys",
			globalStatusFixture{userSettings: `{"global":{"enabled":true}}`, installHooks: true},
			true, map[string]string{"enabled": "true", "agents_covered": "1"},
			[]string{"active_here", "inactive_reason"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setUpGlobalStatus(t, c.fixture)
			var out bytes.Buffer
			if err := runStatus(context.Background(), &out, false, true); err != nil {
				t.Fatal(err)
			}
			gt, ok := decodeGT(t, &out)
			if ok != c.present {
				t.Fatalf("global_tracking present = %v, want %v: %s", ok, c.present, out.String())
			}
			for key, want := range c.keys {
				if got, has := gt[key]; !has || string(got) != want {
					t.Errorf("global_tracking[%q] = %s (present=%v), want %s", key, got, has, want)
				}
			}
			for _, key := range c.absent {
				if _, has := gt[key]; has {
					t.Errorf("global_tracking[%q] must be omitted: %s", key, out.String())
				}
			}
		})
	}
}
