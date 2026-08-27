package cursor

import (
	"context"
	"encoding/json"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}
	count, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if count != 7 {
		t.Errorf("InstallHooks() count = %d, want 7", count)
	}

	hooksFile := readHooksFile(t, tempDir)

	// Verify all hooks are present
	if len(hooksFile.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d, want 1", len(hooksFile.Hooks.SessionStart))
	}
	if len(hooksFile.Hooks.SessionEnd) != 1 {
		t.Errorf("SessionEnd hooks = %d, want 1", len(hooksFile.Hooks.SessionEnd))
	}
	if len(hooksFile.Hooks.BeforeSubmitPrompt) != 1 {
		t.Errorf("BeforeSubmitPrompt hooks = %d, want 1", len(hooksFile.Hooks.BeforeSubmitPrompt))
	}
	if len(hooksFile.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d, want 1", len(hooksFile.Hooks.Stop))
	}
	if len(hooksFile.Hooks.PreCompact) != 1 {
		t.Errorf("PreCompact hooks = %d, want 1", len(hooksFile.Hooks.PreCompact))
	}
	if len(hooksFile.Hooks.SubagentStart) != 1 {
		t.Errorf("SubagentStart hooks = %d, want 1", len(hooksFile.Hooks.SubagentStart))
	}
	if len(hooksFile.Hooks.SubagentStop) != 1 {
		t.Errorf("SubagentStop hooks = %d, want 1", len(hooksFile.Hooks.SubagentStop))
	}

	// Verify version
	if hooksFile.Version != 1 {
		t.Errorf("Version = %d, want 1", hooksFile.Version)
	}

	// Verify commands
	assertEntryCommand(t, hooksFile.Hooks.Stop, agent.WrapProductionSilentHookCommand("entire hooks cursor stop"))
	assertEntryCommand(t, hooksFile.Hooks.SessionStart, agent.WrapProductionSilentHookCommand("entire hooks cursor session-start"))
	assertEntryCommand(t, hooksFile.Hooks.BeforeSubmitPrompt, agent.WrapProductionSilentHookCommand("entire hooks cursor before-submit-prompt"))
	assertEntryCommand(t, hooksFile.Hooks.PreCompact, agent.WrapProductionSilentHookCommand("entire hooks cursor pre-compact"))
	assertEntryCommand(t, hooksFile.Hooks.SubagentStart, agent.WrapProductionSilentHookCommand("entire hooks cursor subagent-start"))
	assertEntryCommand(t, hooksFile.Hooks.SubagentStop, agent.WrapProductionSilentHookCommand("entire hooks cursor subagent-stop"))
}

// TestInstallHooks_WindowsProbeSuccessKeepsShWrappers verifies that on a
// Windows host where a POSIX sh is runnable, Cursor keeps the sh-based wrappers
// (parity with non-Windows). Mutates the shared probe, so no t.Parallel().
func TestInstallHooks_WindowsProbeSuccessKeepsShWrappers(t *testing.T) {
	t.Cleanup(agent.SetWindowsHookProbeForTesting("windows", func(context.Context, string) bool {
		return true // sh works
	}))

	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)
	assertEntryCommand(t, hooksFile.Hooks.SessionStart, agent.WrapProductionSilentHookCommand("entire hooks cursor session-start"))
	assertEntryCommand(t, hooksFile.Hooks.Stop, agent.WrapProductionSilentHookCommand("entire hooks cursor stop"))
}

// TestInstallHooks_WindowsProbeFailureUsesCmdWrappers verifies that on a Windows
// host with no runnable POSIX sh, Cursor installs the native cmd.exe wrappers so
// hooks actually fire (issue #1424). Mutates the shared probe, so no t.Parallel().
func TestInstallHooks_WindowsProbeFailureUsesCmdWrappers(t *testing.T) {
	t.Cleanup(agent.SetWindowsHookProbeForTesting("windows", func(context.Context, string) bool {
		return false // no working sh
	}))

	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)
	assertEntryCommand(t, hooksFile.Hooks.SessionStart, agent.WrapWindowsProductionSilentHookCommand("entire hooks cursor session-start"))
	assertEntryCommand(t, hooksFile.Hooks.Stop, agent.WrapWindowsProductionSilentHookCommand("entire hooks cursor stop"))
	assertEntryCommand(t, hooksFile.Hooks.SubagentStop, agent.WrapWindowsProductionSilentHookCommand("entire hooks cursor subagent-stop"))
}

// TestInstallHooks_WindowsProbeFlipMigratesCleanly verifies that when a host's
// sh availability changes between installs, a non-force reinstall REPLACES the
// stale sh-wrapped hooks with cmd.exe ones rather than leaving both (which would
// double-fire). Mirrors the codex migration test. Mutates the shared probe, so
// no t.Parallel().
func TestInstallHooks_WindowsProbeFlipMigratesCleanly(t *testing.T) {
	shWorks := true
	t.Cleanup(agent.SetWindowsHookProbeForTesting("windows", func(context.Context, string) bool {
		return shWorks
	}))

	tempDir := t.TempDir()
	t.Chdir(tempDir)
	ag := &CursorAgent{}

	// First install with a working sh → sh-based wrappers.
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// sh stops working; reinstall WITHOUT force.
	shWorks = false
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)
	// Exactly one entry per type — the stale sh entry must be gone, not duplicated.
	if len(hooksFile.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d after wrapper migration, want 1 (no duplicate)", len(hooksFile.Hooks.Stop))
	}
	if len(hooksFile.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d after wrapper migration, want 1 (no duplicate)", len(hooksFile.Hooks.SessionStart))
	}
	assertEntryCommand(t, hooksFile.Hooks.Stop, agent.WrapWindowsProductionSilentHookCommand("entire hooks cursor stop"))

	// No sh-based Entire wrapper may survive the migration.
	data, err := os.ReadFile(filepath.Join(tempDir, ".cursor", HooksFileName))
	if err != nil {
		t.Fatalf("failed to read hooks file: %v", err)
	}
	if strings.Contains(string(data), "sh -c") || strings.Contains(string(data), "command -v entire") {
		t.Errorf("stale sh-based wrapper survived migration:\n%s", data)
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	// First install
	count1, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 7 {
		t.Errorf("first InstallHooks() count = %d, want 7", count1)
	}

	// Second install
	count2, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if count2 != 0 {
		t.Errorf("second InstallHooks() count = %d, want 0 (already installed)", count2)
	}

	// Verify no duplicates
	hooksFile := readHooksFile(t, tempDir)
	if len(hooksFile.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d after double install, want 1", len(hooksFile.Hooks.Stop))
	}
}

func TestAreHooksInstalled_NotInstalled(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}
	if hooksInstalledNow(t, ag) {
		t.Error("AreHooksInstalled() = true, want false (no hooks.json)")
	}
}

func TestAreHooksInstalled_AfterInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	_, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if !hooksInstalledNow(t, ag) {
		t.Error("AreHooksInstalled() = false, want true")
	}
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	// Install
	_, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if !hooksInstalledNow(t, ag) {
		t.Fatal("hooks should be installed before uninstall")
	}

	// Uninstall
	err = ag.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	if hooksInstalledNow(t, ag) {
		t.Error("AreHooksInstalled() = true after uninstall, want false")
	}
}

func TestUninstallHooks_NoHooksFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	// Should not error when no hooks file exists
	err := ag.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() should not error when no hooks file: %v", err)
	}
}

// TestUninstallHooks_UnreadableHooksFileErrors pins the absent-vs-unreadable
// split: an absent hooks file means nothing to uninstall, but a read error
// must surface instead of reporting success with hooks still on disk. The
// hooks path is created as a directory so os.ReadFile fails with a
// non-ErrNotExist error on every platform.
func TestUninstallHooks_UnreadableHooksFileErrors(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.MkdirAll(filepath.Join(tempDir, ".cursor", HooksFileName), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if err := (&CursorAgent{}).UninstallHooks(context.Background()); err == nil {
		t.Fatal("UninstallHooks() = nil for unreadable hooks file, want error")
	}
}

func TestInstallHooks_ForceReinstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	// Install normally
	_, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Force reinstall
	count, err := ag.InstallHooks(context.Background(), true)
	if err != nil {
		t.Fatalf("force InstallHooks() error = %v", err)
	}
	if count != 7 {
		t.Errorf("force InstallHooks() count = %d, want 7", count)
	}

	// Verify no duplicates
	hooksFile := readHooksFile(t, tempDir)
	if len(hooksFile.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d after force reinstall, want 1", len(hooksFile.Hooks.Stop))
	}
}

func TestInstallHooks_PreservesExistingHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create hooks file with existing user hooks
	writeHooksFile(t, tempDir, CursorHooksFile{
		Version: 1,
		Hooks: CursorHooks{
			Stop: []CursorHookEntry{
				{Command: "echo user hook"},
			},
			SubagentStop: []CursorHookEntry{
				{Command: "echo file written", Matcher: "Write"},
			},
		},
	})

	ag := &CursorAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)

	// Stop should have user hook + entire hook
	if len(hooksFile.Hooks.Stop) != 2 {
		t.Errorf("Stop hooks = %d, want 2 (user + entire)", len(hooksFile.Hooks.Stop))
	}
	assertEntryCommand(t, hooksFile.Hooks.Stop, "echo user hook")
	assertEntryCommand(t, hooksFile.Hooks.Stop, agent.WrapProductionSilentHookCommand("entire hooks cursor stop"))

	// SubagentStop should have user Write hook + Entire hook
	if len(hooksFile.Hooks.SubagentStop) != 2 {
		t.Errorf("SubagentStop hooks = %d, want 2 (user Write + Entire)", len(hooksFile.Hooks.SubagentStop))
	}
	assertEntryWithMatcher(t, hooksFile.Hooks.SubagentStop, "Write", "echo file written")
	assertEntryCommand(t, hooksFile.Hooks.SubagentStop, agent.WrapProductionSilentHookCommand("entire hooks cursor subagent-stop"))
}

func TestInstallHooks_ReplacesLegacyLocalDevHook(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	ctx := context.Background()
	ag := &CursorAgent{}

	testutil.AssertLegacyHookReplaced(t,
		filepath.Join(tempDir, ".cursor", HooksFileName),
		agent.WrapProductionSilentHookCommandForOS("entire hooks cursor stop", agent.UseWindowsProductionHooks(ctx)),
		testutil.LegacyLocalDevCommand("hooks cursor stop"),
		func() {
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
		})
}

func TestInstallHooks_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create a hooks file with unknown top-level fields and unknown hook types
	existingJSON := `{
  "version": 1,
  "cursorSettings": {"theme": "dark"},
  "hooks": {
    "stop": [{"command": "echo user stop"}],
    "onNotification": [{"command": "echo notify", "filter": "error"}],
    "customHook": [{"command": "echo custom"}]
  }
}`
	cursorDir := filepath.Join(tempDir, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cursorDir, HooksFileName), []byte(existingJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &CursorAgent{}
	count, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 7 {
		t.Errorf("InstallHooks() count = %d, want 7", count)
	}

	// Read the raw JSON to verify unknown fields are preserved
	data, err := os.ReadFile(filepath.Join(cursorDir, HooksFileName))
	if err != nil {
		t.Fatal(err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatal(err)
	}

	// Verify unknown top-level field "cursorSettings" is preserved
	if _, ok := rawFile["cursorSettings"]; !ok {
		t.Error("unknown top-level field 'cursorSettings' was dropped")
	}

	// Verify hooks object contains unknown hook types
	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(rawFile["hooks"], &rawHooks); err != nil {
		t.Fatal(err)
	}

	if _, ok := rawHooks["onNotification"]; !ok {
		t.Error("unknown hook type 'onNotification' was dropped")
	}
	if _, ok := rawHooks["customHook"]; !ok {
		t.Error("unknown hook type 'customHook' was dropped")
	}

	// Verify user's existing stop hook is preserved alongside ours
	var stopHooks []CursorHookEntry
	if err := json.Unmarshal(rawHooks["stop"], &stopHooks); err != nil {
		t.Fatal(err)
	}
	if len(stopHooks) != 2 {
		t.Errorf("stop hooks = %d, want 2 (user + entire)", len(stopHooks))
	}
	assertEntryCommand(t, stopHooks, "echo user stop")
	assertEntryCommand(t, stopHooks, agent.WrapProductionSilentHookCommand("entire hooks cursor stop"))
}

func TestUninstallHooks_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Install hooks first
	ag := &CursorAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}

	// Add unknown fields to the file
	hooksPath := filepath.Join(tempDir, ".cursor", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatal(err)
	}
	rawFile["cursorSettings"] = json.RawMessage(`{"theme":"dark"}`)

	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(rawFile["hooks"], &rawHooks); err != nil {
		t.Fatal(err)
	}
	rawHooks["onNotification"] = json.RawMessage(`[{"command":"echo notify"}]`)
	hooksJSON, err := json.Marshal(rawHooks)
	if err != nil {
		t.Fatal(err)
	}
	rawFile["hooks"] = hooksJSON

	updatedData, err := json.MarshalIndent(rawFile, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, updatedData, 0o644); err != nil {
		t.Fatal(err)
	}

	// Uninstall hooks
	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Read and verify unknown fields are preserved
	data, err = os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatal(err)
	}

	if _, ok := rawFile["cursorSettings"]; !ok {
		t.Error("unknown top-level field 'cursorSettings' was dropped after uninstall")
	}

	if err := json.Unmarshal(rawFile["hooks"], &rawHooks); err != nil {
		t.Fatal(err)
	}

	if _, ok := rawHooks["onNotification"]; !ok {
		t.Error("unknown hook type 'onNotification' was dropped after uninstall")
	}

	// Verify Entire hooks were actually removed
	if hooksInstalledNow(t, ag) {
		t.Error("Entire hooks should be removed after uninstall")
	}
}

// --- Test helpers ---

func readHooksFile(t *testing.T, tempDir string) CursorHooksFile {
	t.Helper()
	hooksPath := filepath.Join(tempDir, ".cursor", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("failed to read "+HooksFileName+": %v", err)
	}

	var hooksFile CursorHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		t.Fatalf("failed to parse "+HooksFileName+": %v", err)
	}
	return hooksFile
}

func writeHooksFile(t *testing.T, tempDir string, hooksFile CursorHooksFile) {
	t.Helper()
	cursorDir := filepath.Join(tempDir, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatalf("failed to create .cursor dir: %v", err)
	}
	data, err := json.MarshalIndent(hooksFile, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal "+HooksFileName+": %v", err)
	}
	hooksPath := filepath.Join(cursorDir, HooksFileName)
	if err := os.WriteFile(hooksPath, data, 0o644); err != nil {
		t.Fatalf("failed to write "+HooksFileName+": %v", err)
	}
}

func assertEntryCommand(t *testing.T, entries []CursorHookEntry, command string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Command == command {
			return
		}
	}
	t.Errorf("hook with command %q not found", command)
}

func assertEntryWithMatcher(t *testing.T, entries []CursorHookEntry, matcher, command string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Matcher == matcher && entry.Command == command {
			return
		}
	}
	t.Errorf("hook with matcher=%q command=%q not found", matcher, command)
}

// TestInstallHooks_DropsLegacyHookAlongsideCurrent is the regression test for
// syncEntireHook returning early when the current command was already present,
// which left a legacy local-dev hook beside it so both fired.
func TestInstallHooks_DropsLegacyHookAlongsideCurrent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	ctx := context.Background()
	ag := &CursorAgent{}

	configPath := filepath.Join(tempDir, ".cursor", HooksFileName)
	current := agent.WrapProductionSilentHookCommandForOS("entire hooks cursor stop", agent.UseWindowsProductionHooks(ctx))
	legacy := testutil.LegacyLocalDevCommand("hooks cursor stop")

	testutil.AssertStaleHookDroppedAlongsideCurrent(t, configPath, current, legacy,
		func() {
			// Install, then append the legacy hook next to the current one.
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("seed InstallHooks() error = %v", err)
			}
			hooksFile := readHooksFile(t, tempDir)
			hooksFile.Hooks.Stop = append(hooksFile.Hooks.Stop, CursorHookEntry{Command: legacy})
			// Write with the same marshaller InstallHooks uses: the production
			// hook command contains `>`, which encoding/json would escape to
			// \u003e, so a seed written with json.MarshalIndent would not match
			// what the CLI reads back.
			data, err := jsonutil.MarshalIndentWithNewline(hooksFile, "", "  ")
			if err != nil {
				t.Fatalf("marshal seed: %v", err)
			}
			if err := os.WriteFile(configPath, data, 0o600); err != nil {
				t.Fatalf("write seed: %v", err)
			}
		},
		func() {
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
		})
}

// TestCommittedDogfoodHooksIsCurrent guards this repo's own committed agent config against drifting from what
// InstallHooks writes. A stale committed config is how the pi extension ended up
// invoking a launcher script that had been deleted.
func TestCommittedDogfoodHooksIsCurrent(t *testing.T) {
	testutil.AssertCommittedDogfoodConfigStable(t, ".cursor/hooks.json", func(t *testing.T, dir string) (int, error) {
		t.Helper()
		t.Chdir(dir)
		return (&CursorAgent{}).InstallHooks(context.Background(), false)
	})
}

// hooksInstalledNow reports whether the agent's hooks are installed, failing the
// test if it could not tell. Built-in agents read a local config file where
// absent means absent, so an error here is a bug, not a state to tolerate.
func hooksInstalledNow(t *testing.T, ag interface {
	AreHooksInstalled(ctx context.Context) (bool, error)
},
) bool {
	t.Helper()

	installed, err := ag.AreHooksInstalled(context.Background())
	if err != nil {
		t.Fatalf("AreHooksInstalled() error = %v", err)
	}
	return installed
}
