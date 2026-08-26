package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
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
