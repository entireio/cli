package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// No t.Parallel in this file: every test uses t.Chdir and/or t.Setenv.

func runTrustCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newTrustCmd()
	// In production the root command carries SilenceErrors (main.go prints);
	// executing the subcommand standalone must match that contract.
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SetContext(t.Context())
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

// TestTrustCmd_NotApplicableForRepoLevelSetup: repo-level setup already IS
// egress consent, so trust must exit 0 with the not-applicable note and write
// nothing — a stray trusted_paths entry would outlive a later repo-level
// disable and resurrect sync without a new consent.
func TestTrustCmd_NotApplicableForRepoLevelSetup(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	t.Cleanup(settings.ClearGlobalModeCache)

	stdout, _, err := runTrustCmd(t)
	if err != nil {
		t.Fatalf("not-applicable must exit 0: %v", err)
	}
	if !strings.Contains(stdout, "not applicable — this repo is explicitly enabled") {
		t.Errorf("missing not-applicable note, got: %q", stdout)
	}
	us, err := settings.LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(us.Global.TrustedOrigins)+len(us.Global.TrustedPaths) != 0 {
		t.Errorf("not-applicable must not record trust, got: %+v", us.Global)
	}
}

// TestTrustCmd_UnconfiguredGlobalTierIsFriendly: the unconfigured-tier
// failure must arrive as guidance (enable --global), not as the raw
// settings-layer error, and as a SilentError so main.go doesn't double-print.
func TestTrustCmd_UnconfiguredGlobalTierIsFriendly(t *testing.T) {
	setupTestRepo(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Cleanup(settings.ClearGlobalModeCache)

	_, stderr, err := runTrustCmd(t)
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("want SilentError (message already printed), got %v", err)
	}
	if !strings.Contains(stderr, "Global tracking is not set up on this machine") ||
		!strings.Contains(stderr, "entire enable --global") {
		t.Errorf("friendly message must explain and point at enable --global, got: %q", stderr)
	}
	if strings.Contains(stderr, "global mode is not configured") {
		t.Errorf("raw settings error must not leak to the user, got: %q", stderr)
	}
}

// TestTrustCmd_PathIdentity: a repo without an origin remote is trusted by
// folder, the output says so, and the gate opens for it.
func TestTrustCmd_PathIdentity(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	t.Cleanup(settings.ClearGlobalModeCache)

	stdout, _, err := runTrustCmd(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "✓ Trusted ") || !strings.Contains(stdout, "(this folder only)") {
		t.Errorf("missing path-scope line, got: %q", stdout)
	}
	if strings.Contains(stdout, "held checkpoint") {
		t.Errorf("no held checkpoints -> no count line, got: %q", stdout)
	}
	if !settings.CheckpointEgressAllowed(t.Context()) {
		t.Error("trust must open the egress gate for this repo")
	}
}

// TestTrustCmd_OriginIdentityAndHeldCount: an origin-keyed repo is trusted
// for all clones, and the held-checkpoint counter (v1 commits not on the
// elected remote, git-branch backend) rides the confirmation.
func TestTrustCmd_OriginIdentityAndHeldCount(t *testing.T) {
	setupTestRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	testutil.AddRemote(t, root, "origin", "https://github.com/acme/widgets.git")
	testutil.WriteFile(t, root, "f.txt", "init")
	testutil.GitAdd(t, root, "f.txt")
	testutil.GitCommit(t, root, "init")
	// One commit on the v1 branch and no remote-tracking ref = one held
	// checkpoint under the git-branch backend.
	testutil.GitUpdateRef(t, root, "refs/heads/entire/checkpoints/v1", "HEAD")
	t.Setenv(settings.EnvCheckpointsPrimary, checkpoint.BackendTypeGitBranch)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)
	t.Cleanup(settings.ClearGlobalModeCache)

	stdout, _, err := runTrustCmd(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "✓ Trusted github.com/acme/widgets (all clones on this machine)") {
		t.Errorf("missing origin-scope line, got: %q", stdout)
	}
	if !strings.Contains(stdout, "1 held checkpoint will sync on your next push.") {
		t.Errorf("missing held-checkpoint count, got: %q", stdout)
	}
}

// TestTrustCmd_RevokeWithdrawsConsent: revoke must close the gate again, say
// it is not retroactive, and stay silent about trust_all when it is off.
func TestTrustCmd_RevokeWithdrawsConsent(t *testing.T) {
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
	if !strings.Contains(stdout, "✓ Revoked trust for ") ||
		!strings.Contains(stdout, "Already-pushed checkpoints stay on the remote.") {
		t.Errorf("missing revoke confirmation, got: %q", stdout)
	}
	if strings.Contains(stdout, "trust_all") {
		t.Errorf("trust_all note must not appear when trust_all is off, got: %q", stdout)
	}
	if settings.CheckpointEgressAllowed(t.Context()) {
		t.Error("revoke must hold checkpoint sync again")
	}
}

// TestTrustCmd_RevokeUnderTrustAllWarnsMasked: with trust_all active a key
// revoke changes nothing — the command must say so instead of implying the
// repo stopped syncing.
func TestTrustCmd_RevokeUnderTrustAllWarnsMasked(t *testing.T) {
	setupTestRepo(t)
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true,"trust_all":true}}`)
	t.Cleanup(settings.ClearGlobalModeCache)

	stdout, _, err := runTrustCmd(t, "--revoke")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Note: trust_all is enabled in settings; this repo will still sync until you disable it.") {
		t.Errorf("missing trust_all masking note, got: %q", stdout)
	}
}

// TestTrustCmd_RevokeUnconfiguredTierIsFriendly: on a never-configured tier
// there was never anything to revoke — printing the full "✓ Revoked … held
// again" confirmation there (the old silent no-op) implied a withdrawal that
// never existed. Revoke must route to the same friendly guidance as grant.
func TestTrustCmd_RevokeUnconfiguredTierIsFriendly(t *testing.T) {
	setupTestRepo(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Cleanup(settings.ClearGlobalModeCache)

	stdout, stderr, err := runTrustCmd(t, "--revoke")
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("want SilentError, got %v", err)
	}
	if strings.Contains(stdout, "✓ Revoked") {
		t.Errorf("must not confirm a revoke on an unconfigured tier, got: %q", stdout)
	}
	if !strings.Contains(stderr, "Global tracking is not set up on this machine") {
		t.Errorf("missing friendly unconfigured-tier message, got: %q", stderr)
	}
}

// TestTrustCmd_OutsideGitRepo: the house prerequisite pattern — friendly
// stderr line plus SilentError, no cobra usage dump.
func TestTrustCmd_OutsideGitRepo(t *testing.T) {
	setupTestDir(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	_, stderr, err := runTrustCmd(t)
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("want SilentError, got %v", err)
	}
	if !strings.Contains(stderr, "Not a git repository") {
		t.Errorf("missing not-a-git-repo message, got: %q", stderr)
	}
}
