package kiro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &KiroAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if count != 5 {
		t.Errorf("InstallHooks() count = %d, want 5", count)
	}

	hooksFile := readHooksFile(t, tempDir)

	// Verify all hooks are present
	if len(hooksFile.Hooks.AgentSpawn) != 1 {
		t.Errorf("AgentSpawn hooks = %d, want 1", len(hooksFile.Hooks.AgentSpawn))
	}
	if len(hooksFile.Hooks.UserPromptSubmit) != 1 {
		t.Errorf("UserPromptSubmit hooks = %d, want 1", len(hooksFile.Hooks.UserPromptSubmit))
	}
	if len(hooksFile.Hooks.Stop) != 1 {
		t.Errorf("Stop hooks = %d, want 1", len(hooksFile.Hooks.Stop))
	}
	if len(hooksFile.Hooks.PreToolUse) != 1 {
		t.Errorf("PreToolUse hooks = %d, want 1", len(hooksFile.Hooks.PreToolUse))
	}
	if len(hooksFile.Hooks.PostToolUse) != 1 {
		t.Errorf("PostToolUse hooks = %d, want 1", len(hooksFile.Hooks.PostToolUse))
	}

	// Verify commands
	assertEntryCommand(t, hooksFile.Hooks.AgentSpawn, "entire hooks kiro agent-spawn")
	assertEntryCommand(t, hooksFile.Hooks.UserPromptSubmit, "entire hooks kiro user-prompt-submit")
	assertEntryCommand(t, hooksFile.Hooks.Stop, "entire hooks kiro stop")
	assertEntryCommand(t, hooksFile.Hooks.PreToolUse, "entire hooks kiro pre-tool-use")
	assertEntryCommand(t, hooksFile.Hooks.PostToolUse, "entire hooks kiro post-tool-use")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &KiroAgent{}

	// First install
	count1, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 5 {
		t.Errorf("first InstallHooks() count = %d, want 5", count1)
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

	ag := &KiroAgent{}
	if ag.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = true, want false (no hooks.json)")
	}
}

func TestAreHooksInstalled_AfterInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &KiroAgent{}

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

	ag := &KiroAgent{}

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

	ag := &KiroAgent{}

	// Should not error when no hooks file exists
	err := ag.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() should not error when no hooks file: %v", err)
	}
}

func TestInstallHooks_ForceReinstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &KiroAgent{}

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
	if count != 5 {
		t.Errorf("force InstallHooks() count = %d, want 5", count)
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
	writeHooksFile(t, tempDir, KiroHooksFile{
		Hooks: KiroHooks{
			Stop: []KiroHookEntry{
				{Command: "echo user hook"},
			},
			PostToolUse: []KiroHookEntry{
				{Command: "echo file written", Matcher: "write_file"},
			},
		},
	})

	ag := &KiroAgent{}
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
	assertEntryCommand(t, hooksFile.Hooks.Stop, "entire hooks kiro stop")

	// PostToolUse should have user hook + Entire hook
	if len(hooksFile.Hooks.PostToolUse) != 2 {
		t.Errorf("PostToolUse hooks = %d, want 2 (user + Entire)", len(hooksFile.Hooks.PostToolUse))
	}
	assertEntryWithMatcher(t, hooksFile.Hooks.PostToolUse, "write_file", "echo file written")
	assertEntryCommand(t, hooksFile.Hooks.PostToolUse, "entire hooks kiro post-tool-use")
}

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &KiroAgent{}
	_, err := ag.InstallHooks(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallHooks(localDev=true) error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)
	assertEntryCommand(t, hooksFile.Hooks.Stop, "go run ${KIRO_PROJECT_DIR}/cmd/entire/main.go hooks kiro stop")
}

func TestInstallHooks_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create a hooks file with unknown top-level fields and unknown hook types
	existingJSON := `{
  "kiroSettings": {"theme": "dark"},
  "hooks": {
    "stop": [{"command": "echo user stop"}],
    "onNotification": [{"command": "echo notify", "filter": "error"}],
    "customHook": [{"command": "echo custom"}]
  }
}`
	kiroDir := filepath.Join(tempDir, ".kiro")
	if err := os.MkdirAll(kiroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kiroDir, HooksFileName), []byte(existingJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &KiroAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 5 {
		t.Errorf("InstallHooks() count = %d, want 5", count)
	}

	// Read the raw JSON to verify unknown fields are preserved
	data, err := os.ReadFile(filepath.Join(kiroDir, HooksFileName))
	if err != nil {
		t.Fatal(err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatal(err)
	}

	// Verify unknown top-level field "kiroSettings" is preserved
	if _, ok := rawFile["kiroSettings"]; !ok {
		t.Error("unknown top-level field 'kiroSettings' was dropped")
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
	var stopHooks []KiroHookEntry
	if err := json.Unmarshal(rawHooks["stop"], &stopHooks); err != nil {
		t.Fatal(err)
	}
	if len(stopHooks) != 2 {
		t.Errorf("stop hooks = %d, want 2 (user + entire)", len(stopHooks))
	}
	assertEntryCommand(t, stopHooks, "echo user stop")
	assertEntryCommand(t, stopHooks, "entire hooks kiro stop")
}

func TestUninstallHooks_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Install hooks first
	ag := &KiroAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Add unknown fields to the file
	hooksPath := filepath.Join(tempDir, ".kiro", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatal(err)
	}
	rawFile["kiroSettings"] = json.RawMessage(`{"theme":"dark"}`)

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

	if _, ok := rawFile["kiroSettings"]; !ok {
		t.Error("unknown top-level field 'kiroSettings' was dropped after uninstall")
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

func readHooksFile(t *testing.T, tempDir string) KiroHooksFile {
	t.Helper()
	hooksPath := filepath.Join(tempDir, ".kiro", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", HooksFileName, err)
	}

	var hooksFile KiroHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		t.Fatalf("failed to parse %s: %v", HooksFileName, err)
	}
	return hooksFile
}

func writeHooksFile(t *testing.T, tempDir string, hooksFile KiroHooksFile) {
	t.Helper()
	kiroDir := filepath.Join(tempDir, ".kiro")
	if err := os.MkdirAll(kiroDir, 0o755); err != nil {
		t.Fatalf("failed to create .kiro dir: %v", err)
	}
	data, err := json.MarshalIndent(hooksFile, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal %s: %v", HooksFileName, err)
	}
	hooksPath := filepath.Join(kiroDir, HooksFileName)
	if err := os.WriteFile(hooksPath, data, 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", HooksFileName, err)
	}
}

func assertEntryCommand(t *testing.T, entries []KiroHookEntry, command string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Command == command {
			return
		}
	}
	t.Errorf("hook with command %q not found", command)
}

func assertEntryWithMatcher(t *testing.T, entries []KiroHookEntry, matcher, command string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Matcher == matcher && entry.Command == command {
			return
		}
	}
	t.Errorf("hook with matcher=%q command=%q not found", matcher, command)
}
