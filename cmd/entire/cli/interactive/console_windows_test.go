//go:build windows

package interactive

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

const consoleProbeHelperEnv = "ENTIRE_TEST_CONSOLE_PROBE_HELPER"

// TestConsoleProbeHelper is the child half of TestConsoleHasNoWindow_Windows: run
// in a process with a chosen console, it prints what the probe sees.
func TestConsoleProbeHelper(t *testing.T) {
	t.Parallel()
	if os.Getenv(consoleProbeHelperEnv) != "1" {
		t.Skip("runs only as TestConsoleHasNoWindow_Windows's child")
	}
	os.Stdout.WriteString("probe=" + strconv.FormatBool(consoleHasNoWindow()) + "\n")
}

// A shell a desktop app starts with CREATE_NO_WINDOW has a console but no
// window, which the probe must report; a console with a window, even a hidden
// one, it must not.
func TestConsoleHasNoWindow_Windows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		attr  *syscall.SysProcAttr
		wantS string
	}{
		{"windowless console", &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}, "probe=true"},
		{"hidden console window", &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}, "probe=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConsoleProbeHelper$", "-test.v")
			cmd.Env = append(os.Environ(), consoleProbeHelperEnv+"=1")
			cmd.SysProcAttr = tt.attr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("helper: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tt.wantS) {
				t.Fatalf("helper output lacks %q:\n%s", tt.wantS, out)
			}
		})
	}
}
