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
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

type statusGlobalTrackingJSON struct {
	GlobalTracking *struct {
		Enabled          bool   `json:"enabled"`
		ActivationSource string `json:"activation_source"`
		ActiveHere       *bool  `json:"active_here"`
		TrustState       string `json:"trust_state"`
		TrustSource      string `json:"trust_source"`
	} `json:"global_tracking"`
}

// TestStatusJSON_GlobalTracking: the global_tracking block reflects the
// derived policy — a repo-enabled repo is gated once the tier is on, and the
// block is absent while the tier is unconfigured. runtime_layout is gone.
func TestStatusJSON_GlobalTracking(t *testing.T) {
	isolatedUserHome(t)
	pretendAgentBinaries(t) // no agents: reconcile has nothing to install
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")
	writeSettings(t, `{"enabled": true}`)

	run := func(t *testing.T) (string, statusGlobalTrackingJSON) {
		t.Helper()
		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
		var parsed statusGlobalTrackingJSON
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("status --json not parseable: %v\n%s", err, out.String())
		}
		return out.String(), parsed
	}

	t.Run("unconfigured tier omits the block", func(t *testing.T) {
		writeUserSettings(t, "")
		raw, parsed := run(t)
		if parsed.GlobalTracking != nil {
			t.Errorf("global_tracking present while unconfigured:\n%s", raw)
		}
	})

	t.Run("repo-enabled repo is untrusted while the tier is on", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":true}}`)
		raw, parsed := run(t)
		if parsed.GlobalTracking == nil {
			t.Fatalf("global_tracking missing:\n%s", raw)
		}
		gt := parsed.GlobalTracking
		if !gt.Enabled || gt.ActivationSource != "local" || gt.ActiveHere == nil || !*gt.ActiveHere {
			t.Errorf("unexpected activation fields: %+v\n%s", gt, raw)
		}
		if gt.TrustState != "untrusted" {
			t.Errorf("trust_state = %q, want untrusted (explicit enable predates the tier):\n%s", gt.TrustState, raw)
		}
		if strings.Contains(raw, "runtime_layout") {
			t.Errorf("runtime_layout must no longer be emitted:\n%s", raw)
		}
	})

	t.Run("trusted origin reads as trusted by repo", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
		raw, parsed := run(t)
		if parsed.GlobalTracking == nil || parsed.GlobalTracking.TrustState != "trusted" || parsed.GlobalTracking.TrustSource != string(settings.TrustSourceRepo) {
			t.Errorf("want trusted/repo, got %+v\n%s", parsed.GlobalTracking, raw)
		}
	})

	t.Run("tier configured but off is not applicable", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":false}}`)
		raw, parsed := run(t)
		if parsed.GlobalTracking == nil || parsed.GlobalTracking.Enabled || parsed.GlobalTracking.TrustState != "not_applicable" {
			t.Errorf("want enabled=false/not_applicable, got %+v\n%s", parsed.GlobalTracking, raw)
		}
	})

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
}
