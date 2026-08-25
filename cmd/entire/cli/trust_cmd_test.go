package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// No t.Parallel in this file: every test uses t.Chdir and/or t.Setenv.

func runTrustCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newTrustCmd()
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SetContext(t.Context())
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

// TestTrustCmd_Refusals: trust must refuse, writing nothing, when the tier is
// not capturing this repo — unconfigured, disabled, or the repo excluded.
func TestTrustCmd_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name         string
		userSettings string // "" = tier never configured
		exclude      bool
		wantStderr   string
	}{
		{"unconfigured tier points at user settings", "", false, "Enable global tracking in"},
		{"disabled tier names the reason", `{"global":{"enabled":false}}`, false, "global tracking is off"},
		{"excluded repo names the exclusion", `{"global":{"enabled":true}}`, true, "excluded in your settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestRepo(t)
			cfg := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", cfg)
			t.Cleanup(settings.ClearGlobalModeCache)
			userSettings := tc.userSettings
			if tc.exclude {
				root, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if resolved, err := filepath.EvalSymlinks(root); err == nil {
					root = resolved
				}
				userSettings = `{"global":{"enabled":true,"exclude_paths_exact":["` + filepath.ToSlash(root) + `"]}}`
			}
			if userSettings != "" {
				writeGlobalUserSettings(t, cfg, userSettings)
			}

			_, stderr, err := runTrustCmd(t)
			var silent *SilentError
			if !errors.As(err, &silent) {
				t.Fatalf("want SilentError (message already printed), got %v", err)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr missing %q, got: %q", tc.wantStderr, stderr)
			}
			if settings.CheckpointEgressAllowed(t.Context()) {
				t.Error("a refusal must not open the gate")
			}
		})
	}
}

func TestTrustCmd_IncidentalRepoSettingsStillUsesGlobalTrust(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Cleanup(settings.ClearGlobalModeCache)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	writeSettings(t, `{"investigate":{"max_turns":4}}`)

	stdout, stderr, err := runTrustCmd(t)
	if err != nil {
		t.Fatalf("trust failed: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "Trusted") {
		t.Fatalf("trust confirmation missing, got: %q", stdout)
	}
	if !settings.CheckpointEgressAllowed(t.Context()) {
		t.Fatal("trust must open egress when repo settings contain no activation intent")
	}
}

// TestTrustCmd_RevokeHonesty: revoke closes the gate and says it is not
// retroactive; under trust_all it must say the revoke is masked.
func TestTrustCmd_RevokeHonesty(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	t.Cleanup(settings.ClearGlobalModeCache)
	if _, _, err := runTrustCmd(t); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runTrustCmd(t, "--revoke")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Already-pushed checkpoints stay on the remote.") {
		t.Errorf("missing revoke confirmation, got: %q", stdout)
	}
	if settings.CheckpointEgressAllowed(t.Context()) {
		t.Error("revoke must hold checkpoint sync again")
	}

	// Under trust_all a key revoke changes nothing and must say so.
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true,"trust_all":true}}`)
	stdout, _, err = runTrustCmd(t, "--revoke")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Note: trust_all is enabled in settings; this repo will still sync until you disable it.") {
		t.Errorf("missing trust_all masking note, got: %q", stdout)
	}
}
