package copilotcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// vsCodeHooksPath is the path to the VS Code hook file under a temp repo root.
func vsCodeHooksPath(tempDir string) string {
	return filepath.Join(tempDir, ".github", "hooks", VSCodeHooksFileName)
}

// readVSCodeFile reads and parses entire-vscode.json from a temp repo root.
func readVSCodeFile(t *testing.T, tempDir string) map[string][]VSCodeHookEntry {
	t.Helper()
	data, err := os.ReadFile(vsCodeHooksPath(tempDir))
	require.NoError(t, err)

	var file struct {
		Version int                          `json:"version"`
		Hooks   map[string][]VSCodeHookEntry `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &file))
	require.Equal(t, 1, file.Version, "version field")
	return file.Hooks
}

func TestInstallVSCodeHooks_FreshInstall(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	ag := &CopilotCLIAgent{}
	count, err := ag.installVSCodeHooks(tempDir, false, false)
	require.NoError(t, err)
	require.Equal(t, 3, count, "user-prompt-submitted + agent-stop + session-end")

	hooks := readVSCodeFile(t, tempDir)

	// Only the two turn events are registered; the rest come from entire.json.
	require.Len(t, hooks, 2, "only UserPromptSubmit and Stop should be registered")
	require.Len(t, hooks[VSCodeEventUserPromptSubmit], 1)
	// VS Code's single Stop event carries both agent-stop and session-end.
	require.Len(t, hooks[VSCodeEventStop], 2)

	ups := hooks[VSCodeEventUserPromptSubmit][0]
	require.Equal(t, "command", ups.Type)
	require.Equal(t, "Entire CLI", ups.Comment)
	require.Equal(t,
		agent.WrapProductionSilentHookCommand("entire hooks copilot-cli user-prompt-submitted"),
		ups.Command,
		"VS Code uses the command field, not bash")

	stopCommands := commandsOf(hooks[VSCodeEventStop])
	require.Contains(t, stopCommands,
		agent.WrapProductionSilentHookCommand("entire hooks copilot-cli agent-stop"))
	require.Contains(t, stopCommands,
		agent.WrapProductionSilentHookCommand("entire hooks copilot-cli session-end"),
		"terminal VS Code Stop must route to session-end")
}

// commandsOf extracts the command strings from a slice of hook entries.
func commandsOf(entries []VSCodeHookEntry) []string {
	cmds := make([]string, len(entries))
	for i, e := range entries {
		cmds[i] = e.Command
	}
	return cmds
}

func TestInstallVSCodeHooks_Idempotent(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	ag := &CopilotCLIAgent{}
	_, err := ag.installVSCodeHooks(tempDir, false, false)
	require.NoError(t, err)
	count, err := ag.installVSCodeHooks(tempDir, false, false)
	require.NoError(t, err)
	require.Equal(t, 0, count, "reinstall adds nothing")

	hooks := readVSCodeFile(t, tempDir)
	require.Len(t, hooks[VSCodeEventUserPromptSubmit], 1, "must not duplicate on reinstall")
	require.Len(t, hooks[VSCodeEventStop], 2, "agent-stop + session-end, no duplicates")
}

func TestInstallVSCodeHooks_PreservesUserHooks(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// A user-managed hook plus an unknown event type and unknown top-level field.
	seed := `{
  "version": 1,
  "customField": "keep-me",
  "hooks": {
    "Stop": [
      {"type": "command", "command": "echo user-stop"}
    ],
    "PreToolUse": [
      {"type": "command", "command": "echo user-pretool"}
    ]
  }
}`
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, hooksDir), 0o755))
	require.NoError(t, os.WriteFile(vsCodeHooksPath(tempDir), []byte(seed), 0o600))

	ag := &CopilotCLIAgent{}
	_, err := ag.installVSCodeHooks(tempDir, false, false)
	require.NoError(t, err)

	hooks := readVSCodeFile(t, tempDir)
	require.Len(t, hooks[VSCodeEventStop], 3, "user Stop hook retained alongside Entire's agent-stop + session-end")
	require.Len(t, hooks[VSCodeEventPreToolUse], 1, "unknown event type preserved")
	require.Equal(t, "echo user-pretool", hooks[VSCodeEventPreToolUse][0].Command)
	require.Contains(t, commandsOf(hooks[VSCodeEventStop]), "echo user-stop", "user Stop hook survives")

	// Unknown top-level field is preserved on round-trip.
	data, err := os.ReadFile(vsCodeHooksPath(tempDir))
	require.NoError(t, err)
	require.Contains(t, string(data), "keep-me")
}

func TestUninstallVSCodeHooks_DeletesWhenEmpty(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	ag := &CopilotCLIAgent{}
	_, err := ag.installVSCodeHooks(tempDir, false, false)
	require.NoError(t, err)
	require.True(t, ag.areVSCodeHooksInstalled(tempDir))

	require.NoError(t, ag.uninstallVSCodeHooks(tempDir))

	_, err = os.Stat(vsCodeHooksPath(tempDir))
	require.ErrorIs(t, err, os.ErrNotExist, "file removed when nothing user-owned remains")
	require.False(t, ag.areVSCodeHooksInstalled(tempDir))
}

func TestUninstallVSCodeHooks_KeepsUserHooks(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	ag := &CopilotCLIAgent{}
	_, err := ag.installVSCodeHooks(tempDir, false, false)
	require.NoError(t, err)

	// Add a user-owned hook to the Stop event.
	rawFile, rawHooks, err := readVSCodeHooksFile(vsCodeHooksPath(tempDir))
	require.NoError(t, err)
	var stop []VSCodeHookEntry
	require.NoError(t, parseVSCodeHookEvent(rawHooks, VSCodeEventStop, &stop))
	stop = append(stop, VSCodeHookEntry{Type: "command", Command: "echo user"})
	require.NoError(t, marshalVSCodeHookEvent(rawHooks, VSCodeEventStop, stop))
	require.NoError(t, writeVSCodeHooksFile(vsCodeHooksPath(tempDir), rawFile, rawHooks))

	require.NoError(t, ag.uninstallVSCodeHooks(tempDir))

	hooks := readVSCodeFile(t, tempDir)
	require.Len(t, hooks[VSCodeEventStop], 1, "user hook survives")
	require.Equal(t, "echo user", hooks[VSCodeEventStop][0].Command)
	require.Empty(t, hooks[VSCodeEventUserPromptSubmit], "Entire turn hook removed")
	require.False(t, ag.areVSCodeHooksInstalled(tempDir))
}

func TestUninstallVSCodeHooks_NoFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	ag := &CopilotCLIAgent{}
	require.NoError(t, ag.uninstallVSCodeHooks(tempDir), "no file is a no-op")
}

func TestInstallVSCodeHooks_ForceReinstall(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	ag := &CopilotCLIAgent{}
	_, err := ag.installVSCodeHooks(tempDir, false, false)
	require.NoError(t, err)
	_, err = ag.installVSCodeHooks(tempDir, false, true)
	require.NoError(t, err)

	hooks := readVSCodeFile(t, tempDir)
	require.Len(t, hooks[VSCodeEventUserPromptSubmit], 1)
	require.Len(t, hooks[VSCodeEventStop], 2, "force reinstall keeps both agent-stop + session-end")
}

func TestUninstallVSCodeHooks_PreservesUserTopLevelFields(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// File with only Entire-managed hooks but a user-added top-level field.
	seed := `{
  "version": 1,
  "customField": "keep-me",
  "hooks": {
    "Stop": [
      {"type": "command", "command": "` + agent.WrapProductionSilentHookCommand("entire hooks copilot-cli agent-stop") + `"}
    ]
  }
}`
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, hooksDir), 0o755))
	require.NoError(t, os.WriteFile(vsCodeHooksPath(tempDir), []byte(seed), 0o600))

	ag := &CopilotCLIAgent{}
	require.NoError(t, ag.uninstallVSCodeHooks(tempDir))

	// File must survive (custom field) but Entire's hook is gone.
	require.FileExists(t, vsCodeHooksPath(tempDir), "file kept because customField is user-owned")
	data, err := os.ReadFile(vsCodeHooksPath(tempDir))
	require.NoError(t, err)
	require.Contains(t, string(data), "keep-me")
	require.False(t, ag.areVSCodeHooksInstalled(tempDir))
}

func TestInstallHooks_AlsoInstallsVSCodeFile(t *testing.T) {
	// No t.Parallel(): t.Chdir mutates process-global cwd.
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	require.FileExists(t, vsCodeHooksPath(tempDir), "InstallHooks must write the VS Code file too")
	require.True(t, ag.AreHooksInstalled(context.Background()))
}

func TestUninstallHooks_AlsoRemovesVSCodeFile(t *testing.T) {
	// No t.Parallel(): t.Chdir mutates process-global cwd.
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &CopilotCLIAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	require.NoError(t, ag.UninstallHooks(context.Background()))
	_, err = os.Stat(vsCodeHooksPath(tempDir))
	require.ErrorIs(t, err, os.ErrNotExist)
}
