//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A CLI ended by TerminateProcess (taskkill /F) takes its plugin with it: the
// kill-on-close job it joined before starting the plugin closes with it.
func TestRunPlugin_PluginDiesWithTerminatedCLI(t *testing.T) {
	t.Parallel()
	ping := filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE")
	if _, err := os.Stat(ping); err != nil {
		t.Skipf("no ping.exe: %v", err)
	}
	helper, pid := startOrphanHelper(t, ping, "-n", "60", "127.0.0.1")
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open plugin process %d: %v", pid, err)
	}
	defer windows.CloseHandle(h)                             //nolint:errcheck // test cleanup
	t.Cleanup(func() { _ = windows.TerminateProcess(h, 1) }) //nolint:errcheck // only if the test failed

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait() //nolint:errcheck // killed on purpose

	ev, err := windows.WaitForSingleObject(h, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if ev != windows.WAIT_OBJECT_0 {
		t.Errorf("plugin %d still running 10s after its CLI was terminated", pid)
	}
}
