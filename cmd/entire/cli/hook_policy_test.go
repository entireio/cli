package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestPrepareHookPolicy pins the derived classification at the hook boundary:
// repo settings first, then the user-global tier, with every failure closed
// except an unreadable user file in a repo-enabled repo (capture stays on,
// egress is held).
func TestPrepareHookPolicy(t *testing.T) {
	const scannersOff = `{"enabled":true,"redaction":{"betterleaks":{"enabled":false},"goredact":{"enabled":false}}}`
	tests := []struct {
		name         string
		repoSettings string // "" = no .entire/settings.json
		userSettings string // "" = no user settings file; "EXCLUDE" = enabled with this repo excluded
		wantErr      bool
		wantActive   bool
		wantSource   repopolicy.ActivationSource
		wantReason   repopolicy.InactiveReason
		wantGitRoot  bool // RuntimeRoot under the git common dir
		wantTrust    repopolicy.TrustReason
	}{
		{name: "no repo settings, tier unconfigured", wantSource: repopolicy.ActivationInactive, wantReason: repopolicy.InactiveReasonGlobalOff},
		{name: "no repo settings, tier off", userSettings: `{"global":{"enabled":false}}`, wantSource: repopolicy.ActivationInactive, wantReason: repopolicy.InactiveReasonGlobalOff},
		{name: "no repo settings, tier on", userSettings: `{"global":{"enabled":true}}`, wantActive: true, wantSource: repopolicy.ActivationGlobal, wantGitRoot: true, wantTrust: repopolicy.TrustReasonUntrusted},
		{name: "repo enabled, tier on", repoSettings: `{"enabled":true}`, userSettings: `{"global":{"enabled":true}}`, wantActive: true, wantSource: repopolicy.ActivationLocal, wantTrust: repopolicy.TrustReasonUntrusted},
		{name: "repo enabled, tier unconfigured", repoSettings: `{"enabled":true}`, wantActive: true, wantSource: repopolicy.ActivationLocal, wantTrust: repopolicy.TrustReasonNone},
		{name: "repo enabled, malformed user settings keeps capture", repoSettings: `{"enabled":true}`, userSettings: `{"global":{"enabled":tru`, wantActive: true, wantSource: repopolicy.ActivationLocal, wantTrust: repopolicy.TrustReasonSettings},
		{name: "repo enabled, scanners off fails closed", repoSettings: scannersOff, userSettings: `{"global":{"enabled":true}}`, wantErr: true},
		{name: "repo disabled vetoes the tier", repoSettings: `{"enabled":false}`, userSettings: `{"global":{"enabled":true}}`, wantSource: repopolicy.ActivationInactive, wantReason: repopolicy.InactiveReasonRepoDisabled},
		{name: "excluded repo", userSettings: "EXCLUDE", wantSource: repopolicy.ActivationInactive, wantReason: repopolicy.InactiveReasonGlobalExcluded},
		{name: "no repo settings, malformed user settings", userSettings: `{"global":{"enabled":tru`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTestRepo(t)
			cfg := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", cfg)
			if tc.repoSettings != "" {
				writeSettings(t, tc.repoSettings)
			}
			body := tc.userSettings
			if body == "EXCLUDE" {
				wd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := filepath.EvalSymlinks(wd)
				if err != nil {
					t.Fatal(err)
				}
				body = `{"global":{"enabled":true,"exclude_paths":["` + filepath.ToSlash(resolved) + `"]}}`
			}
			if body != "" {
				if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			ctx, policy, err := prepareHookPolicy(t.Context())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("prepareHookPolicy = %+v, want error", policy)
				}
				if policy.Active {
					t.Fatalf("policy must be inactive on error, got %+v", policy)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareHookPolicy: %v", err)
			}
			if policy.Active != tc.wantActive || policy.ActivationSource != tc.wantSource {
				t.Fatalf("policy = %+v, want active=%v source=%s", policy, tc.wantActive, tc.wantSource)
			}
			if !tc.wantActive && policy.InactiveReason != tc.wantReason {
				t.Fatalf("inactive reason = %v, want %v", policy.InactiveReason, tc.wantReason)
			}
			if tc.wantActive && policy.Trust.Reason != tc.wantTrust {
				t.Fatalf("trust = %+v, want reason %q", policy.Trust, tc.wantTrust)
			}
			root := policy.RuntimeRoot()
			gitSide := strings.Contains(filepath.ToSlash(root), "/.git/entire/worktree/")
			if gitSide != tc.wantGitRoot {
				t.Fatalf("RuntimeRoot = %q, want git-side=%v", root, tc.wantGitRoot)
			}
			if !tc.wantGitRoot && filepath.Base(root) != ".entire" {
				t.Fatalf("RuntimeRoot = %q, want <worktree>/.entire", root)
			}
			if got, ok := repopolicy.RepoPolicyFromContext(ctx); !ok || !reflect.DeepEqual(got, policy) {
				t.Fatalf("policy snapshot not attached to ctx (ok=%v, got %+v)", ok, got)
			}
		})
	}
}

// TestPrepareHookPolicy_CommittedLocalFileCannotBypassExclusions: a clone that
// ships .entire/settings.local.json {"enabled":true} is not repo-enabled by it,
// so the user's exclude_paths still apply and the hook stays inactive.
func TestPrepareHookPolicy_CommittedLocalFileCannotBypassExclusions(t *testing.T) {
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	settings.ClearVersionedPathCache()
	t.Cleanup(settings.ClearVersionedPathCache)
	local := filepath.Join(dir, settings.EntireSettingsLocalFile)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, dir, "add", "-f", settings.EntireSettingsLocalFile)
	testutil.RunGit(t, dir, "commit", "-m", "ship local settings")

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"),
		[]byte(`{"global":{"enabled":true,"exclude_paths":["`+filepath.ToSlash(resolved)+`"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, policy, err := prepareHookPolicy(t.Context())
	if err != nil {
		t.Fatalf("prepareHookPolicy: %v", err)
	}
	if policy.Active || policy.InactiveReason != repopolicy.InactiveReasonGlobalExcluded {
		t.Fatalf("policy = %+v, want inactive/excluded — a committed local file must not activate", policy)
	}
}

// TestPrepareHookPolicy_CommittedProjectFileCannotBypassExclusions: a clone
// that ships .entire/settings.json (the normal way teams share Entire
// settings) is repository content, and with the tier on the user's
// exclude_paths outrank it — otherwise any third-party clone could activate
// capture, install git hooks, and (under trust_all) sync transcripts from a
// folder the user explicitly excluded.
func TestPrepareHookPolicy_CommittedProjectFileCannotBypassExclusions(t *testing.T) {
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	settings.ClearVersionedPathCache()
	t.Cleanup(settings.ClearVersionedPathCache)
	writeSettings(t, `{}`)
	testutil.RunGit(t, dir, "add", settings.EntireSettingsFile)
	testutil.RunGit(t, dir, "commit", "-m", "ship project settings")

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"),
		[]byte(`{"global":{"enabled":true,"exclude_paths":["`+filepath.ToSlash(resolved)+`"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, policy, err := prepareHookPolicy(t.Context())
	if err != nil {
		t.Fatalf("prepareHookPolicy: %v", err)
	}
	if policy.Active || policy.InactiveReason != repopolicy.InactiveReasonGlobalExcluded {
		t.Fatalf("policy = %+v, want inactive/excluded — a committed project file must not outrank the user's exclusions", policy)
	}
	if policy.Trust.Allowed {
		t.Fatalf("trust = %+v, want egress held for an inactive repo", policy.Trust)
	}

	// The developer's own untracked local file is their action on this clone
	// and keeps the explicit-enable semantics even inside an excluded path.
	local := filepath.Join(dir, settings.EntireSettingsLocalFile)
	if err := os.WriteFile(local, []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	settings.ClearVersionedPathCache()
	_, policy, err = prepareHookPolicy(t.Context())
	if err != nil {
		t.Fatalf("prepareHookPolicy (local override): %v", err)
	}
	if !policy.Active || policy.ActivationSource != repopolicy.ActivationLocal {
		t.Fatalf("policy = %+v, want active/local — an untracked local enable is the user's own choice", policy)
	}
}
