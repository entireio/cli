//go:build darwin || linux

package interactive

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// envJobControlHelper makes the test binary re-exec itself as a helper that
// reports ttyIsPrivateSession() from a process whose session/process-group shape
// this test controls. The shapes are what matters, and a test can only set them
// on a child, not on itself.
const envJobControlHelper = "ENTIRE_TEST_JOBCONTROL_HELPER"

// TestMain serves the helper mode described above before handing off to the
// normal test run.
func TestMain(m *testing.M) {
	if os.Getenv(envJobControlHelper) != "" {
		if ttyIsPrivateSession() {
			os.Stdout.WriteString("true\n")
		} else {
			os.Stdout.WriteString("false\n")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHelper re-execs the test binary with attr applied and returns what
// ttyIsPrivateSession() reported there.
func runHelper(t *testing.T, attr *syscall.SysProcAttr) string {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), self)
	cmd.Env = append(os.Environ(), envJobControlHelper+"=1")
	cmd.SysProcAttr = attr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v (output %q)", err, out)
	}
	return strings.TrimSpace(string(out))
}

// A process given its own session (what setsid + a fresh pty does, and what
// lazygit's output-capturing `git commit` runs inside) is both session and
// process-group leader, so it must report a private session.
func TestTTYIsPrivateSession_OwnSessionIsPrivate(t *testing.T) {
	t.Parallel()

	if got := runHelper(t, &syscall.SysProcAttr{Setsid: true}); got != "true" {
		t.Errorf("ttyIsPrivateSession() in a new session = %s; want true", got)
	}
}

// A shell's job control puts each foreground command in its own process group
// while the session leader stays the shell, so sid != pgrp. That is the shape a
// human's `git commit` has, and it must stay promptable.
func TestTTYIsPrivateSession_ShellJobShapeIsNotPrivate(t *testing.T) {
	t.Parallel()

	if got := runHelper(t, &syscall.SysProcAttr{Setpgid: true}); got != "false" {
		t.Errorf("ttyIsPrivateSession() in a new process group = %s; want false", got)
	}
}
