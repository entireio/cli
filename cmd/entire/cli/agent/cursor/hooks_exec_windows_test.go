package cursor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// TestWindowsInstalledHooks_RunUnderPowerShell installs Cursor's hooks on a
// Windows host with no usable sh, then runs the installed Stop command the way
// Cursor's desktop app runs Windows hooks: through PowerShell
// (https://cursor.com/docs/hooks, "Command-Based Hooks"). The flags Cursor
// passes are not documented; -NoProfile -Command is assumed. A launcher that
// only cmd.exe understands, such as %SystemRoot%\System32\cmd.exe, never starts
// here, so the hook would silently never fire.
func TestWindowsInstalledHooks_RunUnderPowerShell(t *testing.T) {
	// No t.Parallel(): mutates the shared probe and uses t.Chdir/t.Setenv.
	t.Cleanup(agent.SetWindowsHookProbeForTesting("windows", func(context.Context, string) bool {
		return false // no working sh
	}))

	repo := t.TempDir()
	t.Cleanup(osroot.ResetShared) // see TestInstallHooks_WindowsProbeFailureUsesCmdWrappers
	t.Chdir(repo)
	if _, err := (&CursorAgent{}).InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	hooks := readHooksFile(t, repo).Hooks.Stop
	if len(hooks) != 1 {
		t.Fatalf("want one Stop hook, got %d", len(hooks))
	}
	hookCommand := hooks[0].Command

	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	system32 := filepath.Join(sysRoot, "System32")
	stubDir := t.TempDir()
	marker := filepath.Join(stubDir, "entire-ran")
	stub := "@cd >\"%~dp0entire-ran\"\r\n@exit /b 0\r\n"
	if err := os.WriteFile(filepath.Join(stubDir, "entire.bat"), []byte(stub), 0o700); err != nil {
		t.Fatalf("write entire stub: %v", err)
	}
	t.Setenv("PATH", stubDir+";"+system32)

	ps := filepath.Join(system32, "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.CommandContext(t.Context(), ps, "-NoProfile", "-Command", hookCommand)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("installed hook %q failed under PowerShell: %v; stdout=%q stderr=%q", hookCommand, err, stdout.String(), stderr.String())
	}
	ranIn, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("installed hook %q did not start entire under PowerShell (%v); stdout=%q stderr=%q", hookCommand, err, stdout.String(), stderr.String())
	}
	if got := strings.TrimSpace(string(ranIn)); !strings.EqualFold(filepath.Clean(got), filepath.Clean(repo)) {
		t.Fatalf("entire ran in %q, want the repository %q", got, repo)
	}
}
