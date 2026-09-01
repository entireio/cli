package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// writeUserSettingsFileForStatus points the user tier at a per-test dir and
// writes body as its settings.json.
func writeUserSettingsFileForStatus(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A dropped user-settings preference block must be visible in EVERY branch of
// the short status view: the file is machine-wide and its preference blocks
// apply regardless of repo enablement, so the not-set-up and globally-tracked
// branches — which load no repo settings otherwise — must surface it too.
func TestStatus_UserLayerRejectionShownInEveryBranch(t *testing.T) {
	isolatedUserHome(t)
	pretendAgentBinaries(t)

	t.Run("not set up", func(t *testing.T) {
		dir := setupTestDir(t)
		testutil.InitRepo(t, dir)
		writeUserSettingsFileForStatus(t, `{"preferences":{"unknown_key":true}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "not set up") {
			t.Fatalf("expected the not-set-up branch, got:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "user settings:") || !strings.Contains(out.String(), "unknown_key") {
			t.Fatalf("the not-set-up branch must surface the dropped block, got:\n%s", out.String())
		}
	})

	// The rejection must not depend on a full settings load succeeding: a
	// broken repo-side file (here, malformed clone preferences in the git
	// common dir) fails LoadEntireSettings, but the warning is about the
	// machine-wide user file and must still appear.
	t.Run("not set up with broken clone preferences", func(t *testing.T) {
		dir := setupTestDir(t)
		testutil.InitRepo(t, dir)
		if err := os.MkdirAll(filepath.Join(dir, ".git", "entire"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git", "entire", "preferences.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeUserSettingsFileForStatus(t, `{"preferences":{"unknown_key":true}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "user settings:") || !strings.Contains(out.String(), "unknown_key") {
			t.Fatalf("a broken repo-side file must not hide the user-file warning, got:\n%s", out.String())
		}
	})

	t.Run("globally tracked", func(t *testing.T) {
		dir := setupTestDir(t)
		testutil.InitRepo(t, dir)
		testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")
		writeUserSettingsFileForStatus(t, `{"global":{"enabled":true},"preferences":{"unknown_key":true}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "user settings:") || !strings.Contains(out.String(), "unknown_key") {
			t.Fatalf("the globally-tracked branch must surface the dropped block, got:\n%s", out.String())
		}
	})

	t.Run("repo enabled", func(t *testing.T) {
		dir := setupTestDir(t)
		testutil.InitRepo(t, dir)
		writeSettings(t, `{"enabled": true}`)
		writeUserSettingsFileForStatus(t, `{"preferences":{"unknown_key":true}}`)

		var out bytes.Buffer
		if err := runStatus(context.Background(), &out, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "user settings:") || !strings.Contains(out.String(), "unknown_key") {
			t.Fatalf("the enabled branch must surface the dropped block, got:\n%s", out.String())
		}
	})
}
