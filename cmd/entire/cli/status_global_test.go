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

	t.Run("unreadable user settings are reported, not hidden", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":tru`)
		raw, _ := run(t)
		if !strings.Contains(raw, `"settings_error"`) {
			t.Errorf("a broken user settings file must surface in status:\n%s", raw)
		}
	})

	t.Run("unclassifiable repo reports the error, not a fake reason", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":true}}`)
		writeSettings(t, `{"enabled":tru`)
		t.Cleanup(func() { writeSettings(t, `{"enabled": true}`) })
		raw, _ := run(t)
		var parsed struct {
			GlobalTracking struct {
				PolicyError string `json:"policy_error"`
				ActiveHere  *bool  `json:"active_here"`
			} `json:"global_tracking"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.GlobalTracking.PolicyError == "" || parsed.GlobalTracking.ActiveHere != nil {
			t.Errorf("want policy_error set and active_here omitted, got:\n%s", raw)
		}
		if strings.Contains(raw, `"inactive_reason"`) {
			t.Errorf("a classification error must not masquerade as an inactive reason:\n%s", raw)
		}
	})

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
}

// TestStatus_GloballyTrackedRepoWithoutRepoSetup: a repo the user-global tier
// captures, but which was never `entire enable`d, must not read as "not set
// up" — that hint would tell the user to do the very thing global tracking
// exists to make unnecessary, and an agent parsing --json would conclude
// Entire is off while its session is being captured.
func TestStatus_GloballyTrackedRepoWithoutRepoSetup(t *testing.T) {
	isolatedUserHome(t)
	pretendAgentBinaries(t)
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")

	text := func(t *testing.T) string {
		t.Helper()
		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
		return out.String()
	}
	parsed := func(t *testing.T) (string, statusJSON) {
		t.Helper()
		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, true); err != nil {
			t.Fatalf("runStatus --json: %v", err)
		}
		var result statusJSON
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("status --json not parseable: %v\n%s", err, out.String())
		}
		return out.String(), result
	}

	t.Run("tracked repo renders as enabled", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":true}}`)
		out := text(t)
		for _, want := range []string{"● Tracked globally", "branch ", "global tracking: on", agentHelpCommand} {
			if !strings.Contains(out, want) {
				t.Errorf("want %q in status output:\n%s", want, out)
			}
		}
		for _, reject := range []string{"not set up", "entire enable"} {
			if strings.Contains(out, reject) {
				t.Errorf("a globally tracked repo must not print %q:\n%s", reject, out)
			}
		}
		raw, result := parsed(t)
		if !result.Enabled || result.Error != "" || result.AgentHelp != agentHelpCommand {
			t.Errorf("want enabled=true, no error, agent_help set; got:\n%s", raw)
		}
		if result.ActiveSessions == nil || result.Agents == nil {
			t.Errorf("active_sessions and agents must encode as [] not null:\n%s", raw)
		}
		if !strings.Contains(raw, `"activation_source":"global"`) {
			t.Errorf("want activation_source=global:\n%s", raw)
		}
	})

	t.Run("excluded repo stays not set up", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":true,"exclude_paths":["`+filepath.ToSlash(dir)+`"]}}`)
		out := text(t)
		if !strings.Contains(out, "○ not set up") || !strings.Contains(out, "this repo is excluded") {
			t.Errorf("want not-set-up header plus the exclusion reason:\n%s", out)
		}
		raw, result := parsed(t)
		if result.Enabled || result.Error != "not set up" {
			t.Errorf("want enabled=false/error=not set up:\n%s", raw)
		}
	})

	t.Run("tier off stays not set up", func(t *testing.T) {
		writeUserSettings(t, `{"global":{"enabled":false}}`)
		if out := text(t); !strings.Contains(out, "○ not set up") {
			t.Errorf("want not-set-up header:\n%s", out)
		}
	})
}
