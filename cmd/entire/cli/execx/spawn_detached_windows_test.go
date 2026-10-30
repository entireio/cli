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

func TestWindowsDetachedWorkerKeepsDescendantsHeadless(t *testing.T) {
	t.Parallel()

	const helper = "ENTIRE_EXECX_CONSOLE_PROBE"
	switch os.Getenv(helper) {
	case "child":
		getWindow := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
		if err := getWindow.Find(); err != nil {
			t.Fatal(err)
		}
		hwnd, _, _ := getWindow.Call() //nolint:errcheck // GetConsoleWindow returns the window handle, not an error.
		fmt.Fprintf(os.Stdout, "console-window=%d", hwnd)
		os.Exit(0)
	case "worker":
		child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsDetachedWorkerKeepsDescendantsHeadless$")
		child.Env = append(os.Environ(), helper+"=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Run(); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}

	worker := newDetachedCmd(os.Args[0], t.TempDir(), "-test.run=^TestWindowsDetachedWorkerKeepsDescendantsHeadless$")
	worker.Env = append(worker.Env, helper+"=worker")
	out, err := worker.CombinedOutput()
	if err != nil {
		t.Fatalf("background worker failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "console-window=0" {
		t.Fatalf("grandchild allocated a console window: %s", got)
	}
}
