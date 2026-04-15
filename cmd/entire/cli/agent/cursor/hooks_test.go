package cursor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
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

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	// First install
	count1, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 7 {
		t.Errorf("first InstallHooks() count = %d, want 7", count1)
	}

	// Second install
	count2, err := ag.InstallHooks(context.Background(), false, false)
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
	if ag.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = true, want false (no hooks.json)")
	}
}

func TestAreHooksInstalled_AfterInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	_, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if !ag.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = false, want true")
	}
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	// Install
	_, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if !ag.AreHooksInstalled(context.Background()) {
		t.Fatal("hooks should be installed before uninstall")
	}

	// Uninstall
	err = ag.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	if ag.AreHooksInstalled(context.Background()) {
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

func TestInstallHooks_ForceReinstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}

	// Install normally
	_, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Force reinstall
	count, err := ag.InstallHooks(context.Background(), false, true)
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
	_, err := ag.InstallHooks(context.Background(), false, false)
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

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CursorAgent{}
	_, err := ag.InstallHooks(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallHooks(localDev=true) error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)
	assertEntryCommand(t, hooksFile.Hooks.Stop, `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks cursor stop`)
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
	count, err := ag.InstallHooks(context.Background(), false, false)
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
	_, err := ag.InstallHooks(context.Background(), false, false)
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
	if ag.AreHooksInstalled(context.Background()) {
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
