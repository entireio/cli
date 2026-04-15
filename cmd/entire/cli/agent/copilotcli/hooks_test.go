package copilotcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if count != 8 {
		t.Errorf("InstallHooks() count = %d, want 8", count)
	}

	hooksFile := readHooksFile(t, tempDir)

	// Verify all hooks are present
	if len(hooksFile.Hooks.UserPromptSubmitted) != 1 {
		t.Errorf("UserPromptSubmitted hooks = %d, want 1", len(hooksFile.Hooks.UserPromptSubmitted))
	}
	if len(hooksFile.Hooks.SessionStart) != 1 {
		t.Errorf("SessionStart hooks = %d, want 1", len(hooksFile.Hooks.SessionStart))
	}
	if len(hooksFile.Hooks.AgentStop) != 1 {
		t.Errorf("AgentStop hooks = %d, want 1", len(hooksFile.Hooks.AgentStop))
	}
	if len(hooksFile.Hooks.SessionEnd) != 1 {
		t.Errorf("SessionEnd hooks = %d, want 1", len(hooksFile.Hooks.SessionEnd))
	}
	if len(hooksFile.Hooks.SubagentStop) != 1 {
		t.Errorf("SubagentStop hooks = %d, want 1", len(hooksFile.Hooks.SubagentStop))
	}
	if len(hooksFile.Hooks.PreToolUse) != 1 {
		t.Errorf("PreToolUse hooks = %d, want 1", len(hooksFile.Hooks.PreToolUse))
	}
	if len(hooksFile.Hooks.PostToolUse) != 1 {
		t.Errorf("PostToolUse hooks = %d, want 1", len(hooksFile.Hooks.PostToolUse))
	}
	if len(hooksFile.Hooks.ErrorOccurred) != 1 {
		t.Errorf("ErrorOccurred hooks = %d, want 1", len(hooksFile.Hooks.ErrorOccurred))
	}

	// Verify version
	if hooksFile.Version != 1 {
		t.Errorf("Version = %d, want 1", hooksFile.Version)
	}

	// Verify commands use bash field and type is "command"
	assertEntryBash(t, hooksFile.Hooks.UserPromptSubmitted, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli user-prompt-submitted"))
	assertEntryBash(t, hooksFile.Hooks.SessionStart, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli session-start"))
	assertEntryBash(t, hooksFile.Hooks.AgentStop, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli agent-stop"))
	assertEntryBash(t, hooksFile.Hooks.SessionEnd, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli session-end"))
	assertEntryBash(t, hooksFile.Hooks.SubagentStop, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli subagent-stop"))
	assertEntryBash(t, hooksFile.Hooks.PreToolUse, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli pre-tool-use"))
	assertEntryBash(t, hooksFile.Hooks.PostToolUse, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli post-tool-use"))
	assertEntryBash(t, hooksFile.Hooks.ErrorOccurred, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli error-occurred"))

	// Verify type field is "command"
	assertEntryType(t, hooksFile.Hooks.AgentStop, "command")

	// Verify comment field
	assertEntryComment(t, hooksFile.Hooks.AgentStop, "Entire CLI")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}

	// First install
	count1, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 8 {
		t.Errorf("first InstallHooks() count = %d, want 8", count1)
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
	if len(hooksFile.Hooks.AgentStop) != 1 {
		t.Errorf("AgentStop hooks = %d after double install, want 1", len(hooksFile.Hooks.AgentStop))
	}
}

func TestAreHooksInstalled_NotInstalled(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}
	if ag.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = true, want false (no hooks file)")
	}
}

func TestAreHooksInstalled_AfterInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}

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

	ag := &CopilotCLIAgent{}

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

	ag := &CopilotCLIAgent{}

	// Should not error when no hooks file exists
	err := ag.UninstallHooks(context.Background())
	if err != nil {
		t.Fatalf("UninstallHooks() should not error when no hooks file: %v", err)
	}
}

func TestInstallHooks_ForceReinstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}

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
	if count != 8 {
		t.Errorf("force InstallHooks() count = %d, want 8", count)
	}

	// Verify no duplicates
	hooksFile := readHooksFile(t, tempDir)
	if len(hooksFile.Hooks.AgentStop) != 1 {
		t.Errorf("AgentStop hooks = %d after force reinstall, want 1", len(hooksFile.Hooks.AgentStop))
	}
}

func TestInstallHooks_PreservesExistingHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create hooks file with existing user hooks
	writeHooksFile(t, tempDir, CopilotHooksFile{
		Version: 1,
		Hooks: CopilotHooks{
			AgentStop: []CopilotHookEntry{
				{Type: "command", Bash: "echo user hook"},
			},
		},
	})

	ag := &CopilotCLIAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)

	// AgentStop should have user hook + entire hook
	if len(hooksFile.Hooks.AgentStop) != 2 {
		t.Errorf("AgentStop hooks = %d, want 2 (user + entire)", len(hooksFile.Hooks.AgentStop))
	}
	assertEntryBash(t, hooksFile.Hooks.AgentStop, "echo user hook")
	assertEntryBash(t, hooksFile.Hooks.AgentStop, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli agent-stop"))
}

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}
	_, err := ag.InstallHooks(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallHooks(localDev=true) error = %v", err)
	}

	hooksFile := readHooksFile(t, tempDir)
	assertEntryBash(t, hooksFile.Hooks.AgentStop, `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks copilot-cli agent-stop`)
}

func TestInstallHooks_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Create a hooks file with unknown top-level fields and unknown hook types
	existingJSON := `{
  "version": 1,
  "copilotSettings": {"theme": "dark"},
  "hooks": {
    "agentStop": [{"type": "command", "bash": "echo user stop"}],
    "onNotification": [{"type": "command", "bash": "echo notify"}],
    "customHook": [{"type": "command", "bash": "echo custom"}]
  }
}`
	githubHooksDir := filepath.Join(tempDir, ".github", "hooks")
	if err := os.MkdirAll(githubHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubHooksDir, HooksFileName), []byte(existingJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &CopilotCLIAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 8 {
		t.Errorf("InstallHooks() count = %d, want 8", count)
	}

	// Read the raw JSON to verify unknown fields are preserved
	data, err := os.ReadFile(filepath.Join(githubHooksDir, HooksFileName))
	if err != nil {
		t.Fatal(err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatal(err)
	}

	// Verify unknown top-level field "copilotSettings" is preserved
	if _, ok := rawFile["copilotSettings"]; !ok {
		t.Error("unknown top-level field 'copilotSettings' was dropped")
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

	// Verify user's existing agentStop hook is preserved alongside ours
	var agentStopHooks []CopilotHookEntry
	if err := json.Unmarshal(rawHooks["agentStop"], &agentStopHooks); err != nil {
		t.Fatal(err)
	}
	if len(agentStopHooks) != 2 {
		t.Errorf("agentStop hooks = %d, want 2 (user + entire)", len(agentStopHooks))
	}
	assertEntryBash(t, agentStopHooks, "echo user stop")
	assertEntryBash(t, agentStopHooks, agent.WrapProductionSilentHookCommand("entire hooks copilot-cli agent-stop"))
}

func TestUninstallHooks_PreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Install hooks first
	ag := &CopilotCLIAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Add unknown fields to the file
	hooksPath := filepath.Join(tempDir, hooksDir, HooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatal(err)
	}
	rawFile["copilotSettings"] = json.RawMessage(`{"theme":"dark"}`)

	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(rawFile["hooks"], &rawHooks); err != nil {
		t.Fatal(err)
	}
	rawHooks["onNotification"] = json.RawMessage(`[{"type":"command","bash":"echo notify"}]`)
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

	if _, ok := rawFile["copilotSettings"]; !ok {
		t.Error("unknown top-level field 'copilotSettings' was dropped after uninstall")
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

func TestInstallHooks_CreatesDirectoryStructure(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify the .github/hooks/ directory was created
	hooksPath := filepath.Join(tempDir, ".github", "hooks", HooksFileName)
	if _, err := os.Stat(hooksPath); err != nil {
		t.Errorf("expected hooks file at %s, got error: %v", hooksPath, err)
	}
}

func TestInstallHooks_PreservesEntryLevelFields(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir is required for paths.WorktreeRoot.
	tempDir := t.TempDir()

	// Set up a minimal git repo so paths.WorktreeRoot succeeds.
	initGitRepo(t, tempDir)
	t.Chdir(tempDir)

	// Write an existing hooks file with a user entry that has cwd, timeoutSec, and env.
	existingJSON := `{
  "version": 1,
  "hooks": {
    "agentStop": [
      {
        "type": "command",
        "bash": "echo user stop",
        "cwd": "/home/user/project",
        "timeoutSec": 30,
        "env": {
          "MY_VAR": "hello",
          "OTHER": "world"
        }
      }
    ]
  }
}`
	githubHooksDir := filepath.Join(tempDir, ".github", "hooks")
	if err := os.MkdirAll(githubHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubHooksDir, HooksFileName), []byte(existingJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Install hooks (adds Entire entries alongside the user entry).
	ag := &CopilotCLIAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallHooks() installed 0 hooks, expected > 0")
	}

	// Re-read the file and parse hook entries.
	data, err := os.ReadFile(filepath.Join(githubHooksDir, HooksFileName))
	if err != nil {
		t.Fatalf("failed to read hooks file: %v", err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatalf("failed to parse hooks file: %v", err)
	}

	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(rawFile["hooks"], &rawHooks); err != nil {
		t.Fatalf("failed to parse hooks: %v", err)
	}

	var agentStopEntries []CopilotHookEntry
	if err := json.Unmarshal(rawHooks["agentStop"], &agentStopEntries); err != nil {
		t.Fatalf("failed to parse agentStop entries: %v", err)
	}

	// Find the user's entry (not the Entire entry).
	var userEntry *CopilotHookEntry
	for i := range agentStopEntries {
		if agentStopEntries[i].Bash == "echo user stop" {
			userEntry = &agentStopEntries[i]
			break
		}
	}
	require.NotNil(t, userEntry, "user hook entry with bash 'echo user stop' not found after round-trip")

	// Verify cwd is preserved.
	if userEntry.Cwd != "/home/user/project" {
		t.Errorf("cwd = %q, want %q", userEntry.Cwd, "/home/user/project")
	}

	// Verify timeoutSec is preserved.
	if userEntry.TimeoutSec != 30 {
		t.Errorf("timeoutSec = %d, want %d", userEntry.TimeoutSec, 30)
	}

	// Verify env is preserved.
	if len(userEntry.Env) != 2 {
		t.Errorf("env has %d entries, want 2", len(userEntry.Env))
	}
	if userEntry.Env["MY_VAR"] != "hello" {
		t.Errorf("env[MY_VAR] = %q, want %q", userEntry.Env["MY_VAR"], "hello")
	}
	if userEntry.Env["OTHER"] != "world" {
		t.Errorf("env[OTHER] = %q, want %q", userEntry.Env["OTHER"], "world")
	}
}

func TestInstallHooks_MalformedExistingFile(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir is required.
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)
	t.Chdir(tempDir)

	// Write invalid JSON to the hooks file.
	githubHooksDir := filepath.Join(tempDir, ".github", "hooks")
	if err := os.MkdirAll(githubHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubHooksDir, HooksFileName), []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &CopilotCLIAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	if err == nil {
		t.Fatal("InstallHooks() should return error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "failed to parse") {
		t.Errorf("error should mention parsing failure, got: %v", err)
	}
}

func TestUninstallHooks_MalformedExistingFile(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir is required.
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)
	t.Chdir(tempDir)

	githubHooksDir := filepath.Join(tempDir, ".github", "hooks")
	if err := os.MkdirAll(githubHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubHooksDir, HooksFileName), []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &CopilotCLIAgent{}
	err := ag.UninstallHooks(context.Background())
	if err == nil {
		t.Fatal("UninstallHooks() should return error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "failed to parse") {
		t.Errorf("error should mention parsing failure, got: %v", err)
	}
}

func TestUninstallHooks_PreservesUserHooksInManagedTypes(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir is required.
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)
	t.Chdir(tempDir)

	// Write a hooks file with both an Entire hook and a user hook in agentStop.
	existingJSON := `{
  "version": 1,
  "hooks": {
    "agentStop": [
      {
        "type": "command",
        "bash": "entire hooks copilot-cli agent-stop",
        "comment": "Entire CLI"
      },
      {
        "type": "command",
        "bash": "echo user hook",
        "comment": "User custom"
      }
    ]
  }
}`
	githubHooksDir := filepath.Join(tempDir, ".github", "hooks")
	if err := os.MkdirAll(githubHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubHooksDir, HooksFileName), []byte(existingJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &CopilotCLIAgent{}
	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Re-read and verify the user hook survived but the Entire hook was removed.
	data, err := os.ReadFile(filepath.Join(githubHooksDir, HooksFileName))
	if err != nil {
		t.Fatalf("failed to read hooks file: %v", err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		t.Fatalf("failed to parse hooks file: %v", err)
	}

	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(rawFile["hooks"], &rawHooks); err != nil {
		t.Fatalf("failed to parse hooks: %v", err)
	}

	var entries []CopilotHookEntry
	if err := json.Unmarshal(rawHooks["agentStop"], &entries); err != nil {
		t.Fatalf("failed to parse agentStop entries: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (user hook only), got %d: %+v", len(entries), entries)
	}
	if entries[0].Bash != "echo user hook" {
		t.Errorf("remaining entry bash = %q, want %q", entries[0].Bash, "echo user hook")
	}
	if entries[0].Comment != "User custom" {
		t.Errorf("remaining entry comment = %q, want %q", entries[0].Comment, "User custom")
	}
}

// --- Test helpers ---

func readHooksFile(t *testing.T, tempDir string) CopilotHooksFile {
	t.Helper()
	hooksPath := filepath.Join(tempDir, ".github", "hooks", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", HooksFileName, err)
	}

	var hooksFile CopilotHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		t.Fatalf("failed to parse %s: %v", HooksFileName, err)
	}
	return hooksFile
}

func writeHooksFile(t *testing.T, tempDir string, hooksFile CopilotHooksFile) {
	t.Helper()
	githubHooksDir := filepath.Join(tempDir, ".github", "hooks")
	if err := os.MkdirAll(githubHooksDir, 0o755); err != nil {
		t.Fatalf("failed to create .github/hooks dir: %v", err)
	}
	data, err := json.MarshalIndent(hooksFile, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal %s: %v", HooksFileName, err)
	}
	hooksPath := filepath.Join(githubHooksDir, HooksFileName)
	if err := os.WriteFile(hooksPath, data, 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", HooksFileName, err)
	}
}

func assertEntryBash(t *testing.T, entries []CopilotHookEntry, bash string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Bash == bash {
			return
		}
	}
	t.Errorf("hook with bash %q not found", bash)
}

func assertEntryType(t *testing.T, entries []CopilotHookEntry, entryType string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Type == entryType {
			return
		}
	}
	t.Errorf("hook with type %q not found", entryType)
}

func assertEntryComment(t *testing.T, entries []CopilotHookEntry, comment string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Comment == comment {
			return
		}
	}
	t.Errorf("hook with comment %q not found", comment)
}
