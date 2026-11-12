package cursor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// legacyCursorWindowsHooks is .cursor/hooks.json as a version from before the
// PATH-only setting wrote it on a Windows host without a working sh.
func legacyCursorWindowsHooks() string {
	var b strings.Builder
	b.WriteString(`{"version":1,"hooks":{`)
	for i, ht := range cursorHookTypes {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + ht.key + `":[{"command":"cmd.exe /d /s /c \"where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks cursor ` + ht.verb + `)\""}]`)
	}
	b.WriteString(`}}`)
	return b.String()
}

// TestCheckHookConfig_WindowsLegacyWrapperIsOutdated pins that `entire status`
// and `entire doctor` flag Cursor hooks written before the PATH-only setting,
// and that the reinstall they point to clears the warning. Mutates the shared
// probe and the working directory, so no t.Parallel().
func TestCheckHookConfig_WindowsLegacyWrapperIsOutdated(t *testing.T) {
	t.Cleanup(agent.SetWindowsHookProbeForTesting("windows", func(context.Context, string) bool {
		return false // no working sh: the cmd.exe wrapper is what install writes
	}))
	tempDir := t.TempDir()
	t.Cleanup(osroot.ResetShared)
	t.Chdir(tempDir)
	ag := &CursorAgent{}

	if got := ag.CheckHookConfig(context.Background()); got != agent.HooksAbsent {
		t.Fatalf("no hooks file: CheckHookConfig = %v, want absent", got)
	}
	if err := os.MkdirAll(filepath.Join(tempDir, ".cursor"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, ".cursor", HooksFileName), []byte(legacyCursorWindowsHooks()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agent.HooksOutdated {
		t.Fatalf("legacy Windows wrappers: CheckHookConfig = %v, want outdated", got)
	}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agent.HooksCurrent {
		t.Fatalf("after reinstall: CheckHookConfig = %v, want current", got)
	}
}

// TestCheckHookConfig_CurrentAfterInstall covers the host this test runs on:
// a fresh install is current, and losing one hook makes it outdated.
func TestCheckHookConfig_CurrentAfterInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Cleanup(osroot.ResetShared)
	t.Chdir(tempDir)
	ag := &CursorAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agent.HooksCurrent {
		t.Fatalf("after install: CheckHookConfig = %v, want current", got)
	}
	path := filepath.Join(tempDir, ".cursor", HooksFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.Replace(string(data), `"preCompact"`, `"preCompactRenamed"`, 1)
	if err := os.WriteFile(path, []byte(trimmed), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agent.HooksOutdated {
		t.Fatalf("missing preCompact hook: CheckHookConfig = %v, want outdated", got)
	}
}

// TestCheckHookConfig_HostFormWindows installs each form and checks it on each
// host. The sh form is current everywhere, since Cursor's PowerShell runs it
// wherever sh is on PATH; the Windows form is current only on Windows, since sh
// cannot run it. Install-then-check also keeps InstallHooks and CheckHookConfig
// from drifting apart. The check must never run the sh probe, so a slow or
// failed probe cannot flip a healthy install to out of date. Mutates the shared
// probe and the working directory, so no t.Parallel().
func TestCheckHookConfig_HostFormWindows(t *testing.T) {
	for _, tc := range []struct {
		name, installOS, checkOS string
		installSH                bool
		want                     agent.HookConfigState
	}{
		{"sh install checked on linux", "linux", "linux", true, agent.HooksCurrent},
		{"windows install checked on windows", "windows", "windows", false, agent.HooksCurrent},
		{"sh install checked on windows without sh", "linux", "windows", true, agent.HooksCurrent},
		{"windows sh install checked on windows without sh", "windows", "windows", true, agent.HooksCurrent},
		{"windows install checked on linux", "windows", "linux", false, agent.HooksOutdated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Cleanup(osroot.ResetShared)
			t.Chdir(tempDir)
			ag := &CursorAgent{}

			restore := agent.SetWindowsHookProbeForTesting(tc.installOS, func(context.Context, string) bool { return tc.installSH })
			_, err := ag.InstallHooks(context.Background(), false)
			restore()
			if err != nil {
				t.Fatalf("InstallHooks: %v", err)
			}
			t.Cleanup(agent.SetWindowsHookProbeForTesting(tc.checkOS, func(context.Context, string) bool {
				t.Error("CheckHookConfig ran the sh probe")
				return false
			}))
			if got := ag.CheckHookConfig(context.Background()); got != tc.want {
				t.Fatalf("CheckHookConfig = %v, want %v", got, tc.want)
			}
		})
	}
}
