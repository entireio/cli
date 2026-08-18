package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// No t.Parallel in this file: every test uses t.Setenv.

// Warn once per observed-enabled generation; off-note once; trust_all copy.
func TestMaybeWarnGlobalTracking(t *testing.T) {
	for _, tc := range []struct {
		name         string
		userSettings string
		markerBefore bool
		wantContains string
	}{
		{"warns once per enabled generation", `{"global":{"enabled":true}}`, false,
			"Checkpoints sync per repo only after `entire trust`"},
		{"observed off deletes marker and notes held data once", `{"global":{"enabled":false}}`, true,
			"Global tracking is off; locally captured checkpoints in untrusted repos will not sync."},
		{"trust_all variant says capture AND sync", `{"global":{"enabled":true,"trust_all":true}}`, false,
			"captured AND synced (trust_all is enabled)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", cfg)
			writeGlobalUserSettings(t, cfg, tc.userSettings)
			if tc.markerBefore {
				if err := os.WriteFile(filepath.Join(cfg, globalWarnMarkerName), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var first bytes.Buffer
			maybeWarnGlobalTracking(t.Context(), &first)
			if !strings.Contains(first.String(), tc.wantContains) {
				t.Errorf("output missing %q, got: %q", tc.wantContains, first.String())
			}

			first.Reset()
			maybeWarnGlobalTracking(t.Context(), &first)
			if first.Len() != 0 {
				t.Errorf("second observation must be silent, got: %q", first.String())
			}
		})
	}
}

// TestRootCmd_GlobalWarnMarkerSelfFiresOnExplicitCommands drives the real
// root command to pin the marker handshake: enable --global acks the marker
// (no stacked warn), disable --global retires it (no duplicated off-note).
func TestRootCmd_GlobalWarnMarkerSelfFiresOnExplicitCommands(t *testing.T) {
	setupTestRepo(t)
	isolateUserHome(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Cleanup(settings.ClearGlobalModeCache)

	runRoot := func(t *testing.T, args ...string) (stdout, stderr string) {
		t.Helper()
		root := NewRootCmd()
		var out, errBuf bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetArgs(args)
		root.SetContext(t.Context())
		if err := root.Execute(); err != nil {
			t.Fatalf("entire %s: %v\nstderr: %s", strings.Join(args, " "), err, errBuf.String())
		}
		return out.String(), errBuf.String()
	}
	markerPresent := func() bool {
		_, err := os.Stat(filepath.Join(cfg, globalWarnMarkerName))
		return err == nil
	}

	_, stderr := runRoot(t, "enable", "--global")
	if strings.Contains(stderr, "Warning: global tracking is enabled") {
		t.Fatalf("detection warn must not stack on enable --global's own confirmation, got: %q", stderr)
	}
	if !markerPresent() {
		t.Fatal("enable --global must ack the warn marker itself")
	}

	stdout, stderr := runRoot(t, "disable", "--global")
	if got := strings.Count(stdout+stderr, "will not sync"); got != 1 {
		t.Fatalf("held-data consequence must print exactly once, got %d in stdout %q stderr %q", got, stdout, stderr)
	}
	if markerPresent() {
		t.Fatal("disable --global must retire the warn marker itself")
	}
}
