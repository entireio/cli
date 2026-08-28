package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

// repoEnabledWithOrigin: a repo-enabled repo (main's shape) with a GitHub
// origin, as cwd. Not parallel-safe (chdir, env).
func repoEnabledWithOrigin(t *testing.T) {
	t.Helper()
	isolatedUserHome(t)
	pretendAgentBinaries(t)
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")
	writeSettings(t, `{"enabled": true}`)
}

func newTrustTestCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := newTrustCmd()
	cmd.SetContext(context.Background())
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	return cmd, &out, &errOut
}

// TestRunTrust_RepoEnabledWithGlobalOn: once the tier is on, trust applies to
// repo-enabled repos too — grant records the origin key, revoke removes it.
func TestRunTrust_RepoEnabledWithGlobalOn(t *testing.T) {
	repoEnabledWithOrigin(t)
	writeUserSettings(t, `{"global":{"enabled":true}}`)
	cmd, out, _ := newTrustTestCmd()

	if err := runTrust(cmd, false, ""); err != nil {
		t.Fatalf("runTrust: %v", err)
	}
	if !strings.Contains(out.String(), "✓ Trusted github.com/acme/widgets") {
		t.Fatalf("grant output = %q", out.String())
	}
	us, err := settings.LoadUserSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(us.Global.TrustedOrigins, "github.com/acme/widgets") {
		t.Fatalf("trusted_origins = %v, want the repo's origin key", us.Global.TrustedOrigins)
	}

	out.Reset()
	if err := runTrust(cmd, true, ""); err != nil {
		t.Fatalf("runTrust --revoke: %v", err)
	}
	if !strings.Contains(out.String(), "Revoked trust for github.com/acme/widgets") {
		t.Fatalf("revoke output = %q", out.String())
	}
	us, err = settings.LoadUserSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(us.Global.TrustedOrigins, "github.com/acme/widgets") {
		t.Fatalf("trusted_origins still lists the repo after revoke: %v", us.Global.TrustedOrigins)
	}
}

// TestRunTrust_RefusesWhenGlobalOff: a repo-enabled repo is active regardless
// of the tier, but with the tier configured-and-off nothing is gated, so
// trust refuses instead of writing a misleading entry.
func TestRunTrust_RefusesWhenGlobalOff(t *testing.T) {
	repoEnabledWithOrigin(t)
	writeUserSettings(t, `{"global":{"enabled":false}}`)
	settingsPath := filepath.Join(os.Getenv("ENTIRE_CONFIG_DIR"), "settings.json")
	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd, _, errOut := newTrustTestCmd()

	err = runTrust(cmd, false, "")
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("runTrust error = %v, want SilentError", err)
	}
	if !strings.Contains(errOut.String(), "global tracking is off") {
		t.Fatalf("stderr = %q, want the global-off refusal", errOut.String())
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("user settings changed by a refused trust:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestRunTrust_RemoteFlag: a held push to a remote that is about to be
// captured names `entire trust --remote <name>`; the flag records consent for
// that remote's key rather than the currently elected one.
func TestRunTrust_RemoteFlag(t *testing.T) {
	repoEnabledWithOrigin(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	testutil.AddRemote(t, dir, "fork", "https://github.com/me/widgets.git")
	writeUserSettings(t, `{"global":{"enabled":true}}`)
	cmd, out, _ := newTrustTestCmd()

	if err := runTrust(cmd, false, "fork"); err != nil {
		t.Fatalf("runTrust --remote fork: %v", err)
	}
	if !strings.Contains(out.String(), "✓ Trusted github.com/me/widgets") || !strings.Contains(out.String(), "checkpoints sync to fork") {
		t.Fatalf("grant output = %q", out.String())
	}
	us, err := settings.LoadUserSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(us.Global.TrustedOrigins, "github.com/me/widgets") || slices.Contains(us.Global.TrustedOrigins, "github.com/acme/widgets") {
		t.Fatalf("trusted_origins = %v, want only the fork's key", us.Global.TrustedOrigins)
	}

	cmd, _, _ = newTrustTestCmd()
	if err := runTrust(cmd, false, "nope"); err == nil || !strings.Contains(err.Error(), "not a configured git remote") {
		t.Fatalf("want a clear error for an unknown remote, got %v", err)
	}
}
