package factoryaidroid

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// legacyDroidWindowsSettings is .factory/settings.json as a version from
// before the PATH-only setting wrote it on Windows.
func legacyDroidWindowsSettings(t *testing.T) []byte {
	t.Helper()
	legacy := func(verb string) []FactoryHookMatcher {
		return []FactoryHookMatcher{{Hooks: []FactoryHookEntry{{
			Type:    "command",
			Command: `cmd.exe /d /s /c "where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks factoryai-droid ` + verb + `)"`,
		}}}}
	}
	task := func(verb string) []FactoryHookMatcher {
		m := legacy(verb)
		m[0].Matcher = "Task"
		return m
	}
	data, err := json.Marshal(FactorySettings{Hooks: FactoryHooks{
		SessionStart:     legacy(HookNameSessionStart),
		SessionEnd:       legacy(HookNameSessionEnd),
		UserPromptSubmit: legacy(HookNameUserPromptSubmit),
		Stop:             legacy(HookNameStop),
		PreToolUse:       task(HookNamePreToolUse),
		PostToolUse:      task(HookNamePostToolUse),
		PreCompact:       legacy(HookNamePreCompact),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestCheckHookConfig_WindowsLegacyWrapperIsOutdated pins that `entire status`
// and `entire doctor` flag Droid hooks written before the PATH-only setting,
// and that the reinstall they point to clears the warning. Mutates the shared
// probe and the working directory, so no t.Parallel().
func TestCheckHookConfig_WindowsLegacyWrapperIsOutdated(t *testing.T) {
	t.Cleanup(agentpkg.SetWindowsHookProbeForTesting("windows", func(context.Context, string) bool {
		return true
	}))
	tempDir := t.TempDir()
	t.Cleanup(osroot.ResetShared)
	t.Chdir(tempDir)
	ag := &FactoryAIDroidAgent{}

	if got := ag.CheckHookConfig(context.Background()); got != agentpkg.HooksAbsent {
		t.Fatalf("no settings file: CheckHookConfig = %v, want absent", got)
	}
	if err := os.MkdirAll(filepath.Join(tempDir, ".factory"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, ".factory", "settings.json"), legacyDroidWindowsSettings(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agentpkg.HooksOutdated {
		t.Fatalf("legacy Windows wrappers: CheckHookConfig = %v, want outdated", got)
	}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agentpkg.HooksCurrent {
		t.Fatalf("after reinstall: CheckHookConfig = %v, want current", got)
	}
}

// TestCheckHookConfig_TaskHookUnderOtherMatcherIsOutdated covers the one
// placement rule beyond the command text: InstallHooks adds the task hooks
// under the "Task" matcher, so the same command anywhere else would be added
// again on the next install.
func TestCheckHookConfig_TaskHookUnderOtherMatcherIsOutdated(t *testing.T) {
	tempDir := t.TempDir()
	t.Cleanup(osroot.ResetShared)
	t.Chdir(tempDir)
	ag := &FactoryAIDroidAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agentpkg.HooksCurrent {
		t.Fatalf("after install: CheckHookConfig = %v, want current", got)
	}
	path := filepath.Join(tempDir, ".factory", "settings.json")
	settings := readFactorySettings(t, tempDir)
	for i := range settings.Hooks.PreToolUse {
		settings.Hooks.PreToolUse[i].Matcher = "Bash"
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agentpkg.HooksOutdated {
		t.Fatalf("pre-tool-use under another matcher: CheckHookConfig = %v, want outdated", got)
	}
}

// TestCheckHookConfig_OtherHostFormIsCurrentWindows pins that a settings file
// installed on the other OS (a teammate's committed file) reads as current.
// Mutates the shared host and the working directory, so no t.Parallel().
func TestCheckHookConfig_OtherHostFormIsCurrentWindows(t *testing.T) {
	for _, tc := range []struct{ installOS, checkOS string }{
		{"windows", "linux"},
		{"linux", "windows"},
	} {
		t.Run(tc.installOS+" install checked on "+tc.checkOS, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Cleanup(osroot.ResetShared)
			t.Chdir(tempDir)
			ag := &FactoryAIDroidAgent{}
			probe := func(context.Context, string) bool { return true }

			restore := agentpkg.SetWindowsHookProbeForTesting(tc.installOS, probe)
			_, err := ag.InstallHooks(context.Background(), false)
			restore()
			if err != nil {
				t.Fatalf("InstallHooks: %v", err)
			}
			t.Cleanup(agentpkg.SetWindowsHookProbeForTesting(tc.checkOS, probe))
			if got := ag.CheckHookConfig(context.Background()); got != agentpkg.HooksCurrent {
				t.Fatalf("CheckHookConfig = %v, want current", got)
			}
		})
	}
}
