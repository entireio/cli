//go:build integration && !windows

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// SIGINT to the parent must reach the plugin so it can clean up — not
// just be SIGKILL'd by the runtime. Guards both signal paths: terminal
// (via process group) and parent's context-cancel handler.
func TestExternalCommand_SigintReachesPlugin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	signalFile := filepath.Join(dir, "got-sigint.txt")

	// The plugin loops longer than the parent's WaitDelay+grace so that if
	// the signal path were broken, the parent would SIGKILL the child and
	// the marker would never be written. Ready-marker handshake avoids
	// racing SIGINT against shell startup before the trap is installed.
	readyFile := filepath.Join(dir, "ready.txt")
	const pluginLoopSeconds = 10 // > parent WaitDelay (5s) + grace
	body := fmt.Sprintf(
		"#!/bin/sh\ntrap 'echo trapped > %q; exit 130' INT\n"+
			"echo ready > %q\n"+
			"i=0\nwhile [ $i -lt %d ]; do sleep 0.1; i=$((i+1)); done\nexit 0\n",
		signalFile, readyFile, pluginLoopSeconds*10,
	)
	if err := os.WriteFile(filepath.Join(dir, "entire-trapint"), []byte(body), 0o755); err != nil {
		t.Fatalf("write plugin: %v", err)
	}

	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "trapint")
	cmd.Env = pathWith(dir)
	var pStderr bytes.Buffer
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &pStderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	if !waitForFile(readyFile, 3*time.Second) {
		if killErr := cmd.Process.Kill(); killErr != nil {
			t.Logf("kill process: %v", killErr)
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			t.Logf("wait after kill: %v", waitErr)
		}
		t.Fatalf("plugin never reached ready state\nparent stderr:\n%s", pStderr.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal parent: %v", err)
	}

	if !waitForFile(signalFile, 5*time.Second) {
		if waitErr := cmd.Wait(); waitErr != nil {
			t.Logf("wait after signal: %v", waitErr)
		}
		t.Fatalf("plugin never observed SIGINT — marker missing\nparent stderr:\n%s", pStderr.String())
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		t.Logf("wait: %v", waitErr)
	}

	contents, err := os.ReadFile(signalFile)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got := strings.TrimSpace(string(contents)); got != "trapped" {
		t.Errorf("marker = %q, want %q", got, "trapped")
	}
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A signal Entire received outranks the one its child died of.
//
// Cancelling the context makes runPlugin send the plugin SIGINT whatever
// Entire itself was sent, so the child's signal is often Entire's own signal
// laundered — and laundered lossily. A supervisor's SIGTERM must still leave
// Entire dying of SIGTERM (143), not of whatever the child ended up with:
// this plugin ignores SIGINT, so it outlives WaitDelay and os/exec SIGKILLs
// it, which reported 137 before the precedence was fixed.
func TestExternalCommand_ParentsSignalOutranksTheChilds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready.txt")
	// Longer than the parent's WaitDelay (5s) plus grace, so the child is
	// still alive when the delay expires and is killed rather than exiting.
	const pluginLoopSeconds = 20
	body := fmt.Sprintf(
		"#!/bin/sh\ntrap '' INT\n"+
			"echo ready > %q\n"+
			"i=0\nwhile [ $i -lt %d ]; do sleep 0.1; i=$((i+1)); done\nexit 0\n",
		readyFile, pluginLoopSeconds*10,
	)
	if err := os.WriteFile(filepath.Join(dir, "entire-ignoreint"), []byte(body), 0o755); err != nil {
		t.Fatalf("write plugin: %v", err)
	}

	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "ignoreint")
	cmd.Env = pathWith(dir)
	var pStderr bytes.Buffer
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &pStderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !waitForFile(readyFile, 5*time.Second) {
		if killErr := cmd.Process.Kill(); killErr != nil {
			t.Logf("kill process: %v", killErr)
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			t.Logf("wait after kill: %v", waitErr)
		}
		t.Fatalf("plugin never reached ready state\nparent stderr:\n%s", pStderr.String())
	}

	// A supervisor or container stop, not a terminal Ctrl-C: only the parent
	// is signalled, and with SIGTERM rather than SIGINT.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal parent: %v", err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatalf("parent exited 0 after SIGTERM\nparent stderr:\n%s", pStderr.String())
	}

	// Re-raised, so the parent is genuinely WIFSIGNALED: an os.Exit(143) would
	// not break an enclosing shell loop.
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("no wait status: %v", waitErr)
	}
	if !ws.Signaled() {
		t.Fatalf("parent exited %d rather than dying from a signal\nparent stderr:\n%s",
			cmd.ProcessState.ExitCode(), pStderr.String())
	}
	if ws.Signal() != syscall.SIGTERM {
		t.Errorf("parent died of %v, want SIGTERM — the child's signal (SIGKILL here) must not outrank ours\nparent stderr:\n%s",
			ws.Signal(), pStderr.String())
	}
}
