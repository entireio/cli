package globalhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const launcherChildEnv = "ENTIRE_GLOBALHOOK_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(launcherChildEnv) == "1" {
		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(18)
		}
		fmt.Fprintln(os.Stdout, strings.Join(os.Args[1:], "|"))
		fmt.Fprint(os.Stdout, string(payload))
		fmt.Fprintln(os.Stderr, "selected stderr")
		os.Exit(17)
	}
	os.Exit(m.Run())
}

func TestWindowsLauncherForwardsNativeStreams(t *testing.T) {
	t.Parallel()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	selectedPath := filepath.Join(t.TempDir(), "Entire 雪 ' 100%.exe")
	if err := os.WriteFile(selectedPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	selection, err := New(selectedPath)
	if err != nil {
		t.Fatal(err)
	}
	systemDir, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	command := selection.Command("claude-code", "stop")
	const payload = `{"session_id":"unicode-雪","prompt":"hello"}`
	run := func() (string, string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		shell := filepath.Join(systemDir, "cmd.exe")
		cmd := exec.CommandContext(ctx, shell)
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + shell + `" /d /s /c "` + command + `"`}
		cmd.Env = append(os.Environ(), launcherChildEnv+"=1")
		cmd.Stdin = strings.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	stdout, stderr, err := run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 {
		t.Fatalf("exit status = %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
	if stdout != "hooks|global|claude-code|stop\n"+payload || stderr != "selected stderr\n" {
		t.Fatalf("native streams changed: stdout=%q stderr=%q", stdout, stderr)
	}
	if err := os.Remove(selectedPath); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = run()
	if err != nil || stdout != "" || !strings.Contains(stderr, "selected hook installation is unavailable") {
		t.Fatalf("missing selected executable = %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
}
