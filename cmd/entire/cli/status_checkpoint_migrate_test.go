package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// entireStatusRepo is a repo with origin (GitHub) plus an Entire remote, so the
// election lands on the Entire tier. Tests change the process cwd and cannot
// run in parallel.
func entireStatusRepo(t *testing.T) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := setupTestRepo(t)
	testutil.WriteFile(t, dir, "README.md", "hello")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/app.git")
	testutil.AddRemote(t, dir, "entire", "entire://cluster.test/gh/acme/app")
	return dir
}

func addLocalCheckpointRef(t *testing.T, dir string) {
	t.Helper()
	head := testutil.GetHeadHash(t, dir)
	testutil.RunGit(t, dir, "update-ref", "refs/entire/checkpoints/ab/0123456789ab", head)
}

func writeEntireSyncLedger(t *testing.T, migration string) {
	t.Helper()
	body := `{"remote":"entire","migration":"` + migration + `"}`
	if err := os.WriteFile(filepath.Join(".git", "entire-checkpoint-sync-entire.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

func TestRunStatus_CheckpointSyncDestination_EntireAnnotated(t *testing.T) {
	entireStatusRepo(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Checkpoints sync to: entire (your Entire remote)") {
		t.Errorf("expected the Entire annotation, got:\n%s", out)
	}
	if strings.Contains(out, "older checkpoints may still be on") {
		t.Errorf("no local checkpoints means nothing to nudge about, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncNudge_PendingWithLocalCheckpoints(t *testing.T) {
	dir := entireStatusRepo(t)
	addLocalCheckpointRef(t, dir)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	out := stdout.String()
	want := "older checkpoints may still be on origin — run 'entire checkpoint migrate' to bring them over"
	if !strings.Contains(out, want) {
		t.Errorf("expected the nudge %q, got:\n%s", want, out)
	}
}

func TestRunStatus_CheckpointSyncNudge_AbsentWhenLedgerDone(t *testing.T) {
	dir := entireStatusRepo(t)
	addLocalCheckpointRef(t, dir)
	writeEntireSyncLedger(t, "done")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	if out := stdout.String(); strings.Contains(out, "older checkpoints may still be on") {
		t.Errorf("a recorded migration must silence the nudge, got:\n%s", out)
	}
}

func TestRunStatusJSON_CheckpointSync_Migration(t *testing.T) {
	dir := entireStatusRepo(t)
	addLocalCheckpointRef(t, dir)

	decode := func() statusJSON {
		var stdout bytes.Buffer
		if err := runStatus(context.Background(), &stdout, false, true); err != nil {
			t.Fatalf("runStatus() error = %v", err)
		}
		var result statusJSON
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("json.Unmarshal() error = %v\n%s", err, stdout.String())
		}
		return result
	}

	got := decode()
	if got.CheckpointSyncRemote != "entire" || got.CheckpointSyncRemoteSource != "entire" {
		t.Errorf("expected the Entire election, got %+v", got)
	}
	if got.CheckpointSyncMigration != checkpointSyncMigrationPending {
		t.Errorf("checkpoint_sync_migration = %q, want pending", got.CheckpointSyncMigration)
	}

	writeEntireSyncLedger(t, "declined")
	if got := decode(); got.CheckpointSyncMigration != checkpointSyncMigrationDeclined {
		t.Errorf("checkpoint_sync_migration = %q, want declined", got.CheckpointSyncMigration)
	}
}

func TestRunStatusJSON_CheckpointSync_MigrationOmittedForOtherSources(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	if strings.Contains(stdout.String(), "checkpoint_sync_migration") {
		t.Errorf("the ledger field belongs to the Entire tier only, got:\n%s", stdout.String())
	}
}
