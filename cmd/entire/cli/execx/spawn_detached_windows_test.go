//go:build windows

package execx

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const consoleProbeEnv = "ENTIRE_EXECX_CONSOLE_PROBE"

// The worker re-execs this test as a child, and that child as a grandchild
// that reports its console window. A worker with no console would make the
// grandchild allocate a visible one.
func TestWindowsDetachedWorkerKeepsDescendantsHeadless(t *testing.T) {
	t.Parallel()

	const run = "-test.run=^TestWindowsDetachedWorkerKeepsDescendantsHeadless$"
	switch os.Getenv(consoleProbeEnv) {
	case "child":
		getWindow := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
		if err := getWindow.Find(); err != nil {
			t.Fatal(err)
		}
		hwnd, _, _ := getWindow.Call() //nolint:errcheck // GetConsoleWindow returns the window handle, not an error.
		fmt.Fprintf(os.Stdout, "console-window=%d\n", hwnd)
		return
	case "worker":
		child := exec.CommandContext(t.Context(), os.Args[0], run)
		child.Env = append(os.Environ(), consoleProbeEnv+"=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}

	worker := newDetachedCmd(os.Args[0], t.TempDir(), run)
	worker.Env = append(worker.Env, consoleProbeEnv+"=worker")
	out, err := worker.CombinedOutput()
	if err != nil {
		t.Fatalf("background worker failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "console-window=0\n") {
		t.Fatalf("grandchild allocated a console window:\n%s", out)
	}
}
