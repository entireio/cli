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

func writeWarnMarker(t *testing.T, cfg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg, globalWarnMarkerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestMaybeWarnGlobalTracking_WarnsOncePerGeneration: the detection warn must
// fire on the first foreground command that observes the tier enabled and
// then stay silent — a warn on every command would train users to ignore it.
// The marker must land under ENTIRE_CONFIG_DIR, beside settings.json.
func TestMaybeWarnGlobalTracking_WarnsOncePerGeneration(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true}}`)

	var first bytes.Buffer
	maybeWarnGlobalTracking(t.Context(), &first)
	if !strings.Contains(first.String(), "Warning: global tracking is enabled") {
		t.Fatalf("first observed-enabled command must warn, got: %q", first.String())
	}
	if !strings.Contains(first.String(), "Checkpoints sync per repo only after `entire trust`") {
		t.Errorf("warn must carry the per-repo trust pointer, got: %q", first.String())
	}
	if _, err := os.Stat(filepath.Join(cfg, globalWarnMarkerName)); err != nil {
		t.Fatalf("marker not written under ENTIRE_CONFIG_DIR: %v", err)
	}

	var second bytes.Buffer
	maybeWarnGlobalTracking(t.Context(), &second)
	if second.Len() != 0 {
		t.Errorf("second observed-enabled command must be silent, got: %q", second.String())
	}
}

// TestMaybeWarnGlobalTracking_ObservedOffDeletesMarkerAndNotesHeldData: a
// disable that bypassed `disable --global` (hand-edit) still owes the user
// the held-data one-liner, exactly once, while the marker is retired.
func TestMaybeWarnGlobalTracking_ObservedOffDeletesMarkerAndNotesHeldData(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":false}}`)
	writeWarnMarker(t, cfg)

	var out bytes.Buffer
	maybeWarnGlobalTracking(t.Context(), &out)
	if !strings.Contains(out.String(), "Global tracking is off; locally captured checkpoints in untrusted repos will not sync.") {
		t.Fatalf("missing off note, got: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(cfg, globalWarnMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("observed-off must delete the marker, stat err: %v", err)
	}

	var again bytes.Buffer
	maybeWarnGlobalTracking(t.Context(), &again)
	if again.Len() != 0 {
		t.Errorf("off note must print once per generation, got: %q", again.String())
	}
}

// The off→on re-warn is emergent from the two mechanics above (observed-off
// deletes the marker; enabled-without-marker warns) — no separate test.

// TestMaybeWarnGlobalTracking_TrustAllVariant: with trust_all set the
// per-repo "sync only after `entire trust`" sentence would lie — the warn
// must say capture AND sync instead.
func TestMaybeWarnGlobalTracking_TrustAllVariant(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":{"enabled":true,"trust_all":true}}`)

	var out bytes.Buffer
	maybeWarnGlobalTracking(t.Context(), &out)
	if !strings.Contains(out.String(), "captured AND synced (trust_all is enabled)") {
		t.Fatalf("missing trust_all variant, got: %q", out.String())
	}
	if strings.Contains(out.String(), "only after `entire trust`") {
		t.Errorf("per-repo sentence would lie under trust_all, got: %q", out.String())
	}
}

// TestMaybeWarnGlobalTracking_UnreadableSettingsSilent: a malformed settings
// file is doctor's surface; warning here would fire on every command forever
// (no readable state to hang a marker decision on). The marker must survive
// so a repaired file doesn't spuriously re-warn.
func TestMaybeWarnGlobalTracking_UnreadableSettingsSilent(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	writeGlobalUserSettings(t, cfg, `{"global":`) // truncated JSON
	writeWarnMarker(t, cfg)

	var out bytes.Buffer
	maybeWarnGlobalTracking(t.Context(), &out)
	if out.Len() != 0 {
		t.Errorf("unreadable settings must stay silent, got: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(cfg, globalWarnMarkerName)); err != nil {
		t.Errorf("unreadable settings must not touch the marker: %v", err)
	}
}

// TestRootCmd_GlobalWarnMarkerSelfFiresOnExplicitCommands drives the real
// root command end to end (RunE + PersistentPostRun) to pin the marker
// handshake the isolated-function tests cannot see:
//
//   - `enable --global`'s own confirmation IS the announcement, so it acks
//     the marker itself — the detection warn must not stack on top of it, in
//     that invocation or the next foreground command;
//   - `disable --global`'s held-data line replaces the off-note, so it
//     retires the marker itself — the off-note must not duplicate it, in that
//     invocation or the next foreground command.
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

	stdout, stderr := runRoot(t, "enable", "--global")
	if !strings.Contains(stdout, "Global tracking enabled.") {
		t.Fatalf("enable --global confirmation missing, got: %q", stdout)
	}
	if strings.Contains(stderr, "Warning: global tracking is enabled") {
		t.Fatalf("detection warn must not stack on enable --global's own confirmation, got: %q", stderr)
	}
	if !markerPresent() {
		t.Fatal("enable --global must ack the warn marker itself")
	}

	if _, stderr := runRoot(t, "status"); strings.Contains(stderr, "Warning: global tracking is enabled") {
		t.Fatalf("the command after enable --global must not warn either, got: %q", stderr)
	}

	stdout, stderr = runRoot(t, "disable", "--global")
	if !strings.Contains(stdout, "Locally captured checkpoints in untrusted repos will not sync.") {
		t.Fatalf("disable --global held-data line missing, got: %q", stdout)
	}
	if strings.Contains(stderr, "Global tracking is off;") {
		t.Fatalf("off-note must not duplicate disable --global's own line, got: %q", stderr)
	}
	if got := strings.Count(stdout+stderr, "will not sync"); got != 1 {
		t.Fatalf("held-data consequence must print exactly once, got %d in stdout %q stderr %q", got, stdout, stderr)
	}
	if markerPresent() {
		t.Fatal("disable --global must retire the warn marker itself")
	}

	if _, stderr := runRoot(t, "status"); strings.Contains(stderr, "Global tracking is off;") {
		t.Fatalf("the command after disable --global must not print the off-note, got: %q", stderr)
	}
}
