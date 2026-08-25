package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
