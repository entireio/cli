package factoryaidroid

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// legacyDroidCommand turns a current Windows command back into the form a
// released version from before the PATH-only setting wrote: a bare cmd.exe
// launcher, no `set …& pushd …& ` before the where.exe guard and no `popd& `
// opening the branches.
func legacyDroidCommand(command string) string {
	command = strings.Replace(command, `%SystemRoot%\System32\cmd.exe /d /s /c "`, `cmd.exe /d /s /c "`, 1)
	command = strings.Replace(command, "set NoDefaultCurrentDirectoryInExePath=1& pushd %SystemRoot%& ", "", 1)
	return strings.ReplaceAll(command, "popd& ", "")
}

// TestCheckHookConfig_WindowsLegacyWrapperIsOutdated pins that `entire status`
// and `entire doctor` flag Droid hooks written before the PATH-only setting,
// and that the reinstall they point to replaces every one of them. The legacy
// file is a Windows install with each command turned back into its released
// form, so the hooks are all present and only the wrappers differ. Mutates the
// shared host and the working directory, so no t.Parallel().
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
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	settings := readFactorySettings(t, tempDir)
	wrappers := 0
	for _, l := range droidHookLists(&settings.Hooks, droidHookCommands{}) {
		for i := range *l.matchers {
			for j := range (*l.matchers)[i].Hooks {
				hook := &(*l.matchers)[i].Hooks[j]
				legacy := legacyDroidCommand(hook.Command)
				if legacy == hook.Command {
					t.Fatalf("%s: %q is not a current Windows wrapper", l.key, hook.Command)
				}
				hook.Command = legacy
				wrappers++
			}
		}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tempDir, ".factory", "settings.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
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
	settings = readFactorySettings(t, tempDir)
	after := 0
	for _, l := range droidHookLists(&settings.Hooks, droidHookCommands{}) {
		for _, m := range *l.matchers {
			for _, hook := range m.Hooks {
				after++
				if legacyDroidCommand(hook.Command) == hook.Command {
					t.Errorf("%s: legacy wrapper survived the reinstall: %q", l.key, hook.Command)
				}
			}
		}
	}
	if after != wrappers {
		t.Errorf("after reinstall: %d hooks, want %d", after, wrappers)
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

// TestCheckHookConfig_HostFormWindows pins that only this host's form is
// current: droid runs hooks under sh on macOS and Linux and under cmd.exe on
// Windows, and neither runs the other's form, so a settings file committed
// from the other OS is outdated. Install-then-check on the same host also
// keeps InstallHooks and CheckHookConfig from drifting apart. Mutates the
// shared host and the working directory, so no t.Parallel().
func TestCheckHookConfig_HostFormWindows(t *testing.T) {
	for _, tc := range []struct {
		installOS, checkOS string
		want               agentpkg.HookConfigState
	}{
		{"linux", "linux", agentpkg.HooksCurrent},
		{"windows", "windows", agentpkg.HooksCurrent},
		{"windows", "linux", agentpkg.HooksOutdated},
		{"linux", "windows", agentpkg.HooksOutdated},
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
			if got := ag.CheckHookConfig(context.Background()); got != tc.want {
				t.Fatalf("CheckHookConfig = %v, want %v", got, tc.want)
			}
		})
	}
}
