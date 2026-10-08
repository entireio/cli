package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The auto-enable that fires when a user picks an external agent has to land
// somewhere the loader will honor. Written into the version-controlled
// .entire/settings.json it is dropped on the next load, so the agent the user
// just chose silently stops being discovered.
func TestEnableExternalAgentsLocally_TakesEffect(t *testing.T) { //nolint:paralleltest // t.Chdir
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatalf("write project settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"),
		[]byte(`{"log_level":"debug"}`), 0o644); err != nil {
		t.Fatalf("write local settings: %v", err)
	}
	t.Chdir(dir)

	grant, err := enableExternalAgentsLocally(t.Context())
	if err != nil {
		t.Fatalf("enableExternalAgentsLocally: %v", err)
	}
	if !grant.Effective {
		t.Errorf("grant reported ineffective (%s), want it honored in an untracked local file", grant.Reason)
	}

	loaded, err := settings.Load(t.Context())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !loaded.ExternalAgents {
		reason, _ := loaded.ExternalAgentsRejection()
		t.Errorf("external_agents did not take effect (rejection: %q)", reason)
	}
	if loaded.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want the existing local setting preserved", loaded.LogLevel)
	}
}

// TestEnableExternalAgentsLocally_TrackedLocalFile_ReportsIneffective covers
// the second route to the symptom commit 51d062a34 fixed: the write succeeds,
// but the grant it wrote can never be read.
//
// A TRACKED .entire/settings.local.json makes loadMergedSettings drop the whole
// local layer, so the key lands in a file the loader ignores. Worse, the gate
// that would normally record a rejection never runs: enforceExternalAgentsTrust
// returns early on !s.ExternalAgents, which is exactly the state a dropped
// layer produces. So nothing anywhere says external_agents is inert, while the
// caller prints a notice saying it is on.
func TestEnableExternalAgentsLocally_TrackedLocalFile_ReportsIneffective(t *testing.T) { //nolint:paralleltest // t.Chdir
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatalf("write project settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"),
		[]byte(`{"log_level":"debug"}`), 0o644); err != nil {
		t.Fatalf("write local settings: %v", err)
	}
	// -f because .entire/settings.local.json is gitignored; committing it
	// anyway is precisely the state this guards against.
	testutil.GitAddForce(t, dir, ".entire/settings.local.json")
	testutil.GitCommit(t, dir, "track local settings")
	t.Chdir(dir)

	grant, err := enableExternalAgentsLocally(t.Context())
	if err != nil {
		t.Fatalf("enableExternalAgentsLocally: %v", err)
	}

	if grant.Effective {
		t.Error("grant reported effective, but a tracked local file is never read back")
	}
	if grant.Reason == "" {
		t.Error("an ineffective grant must carry a reason the caller can show the user")
	}

	s, err := settings.Load(t.Context())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.ExternalAgents {
		t.Fatal("precondition failed: tracked local layer should not be honored")
	}
}
