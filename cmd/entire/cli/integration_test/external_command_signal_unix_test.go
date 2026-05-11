//go:build integration && !windows

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if err := os.WriteFile(filepath.Join(dir, "entire-trapint"), []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
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
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("plugin never reached ready state\nparent stderr:\n%s", pStderr.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal parent: %v", err)
	}

	if !waitForFile(signalFile, 5*time.Second) {
		_ = cmd.Wait()
		t.Fatalf("plugin never observed SIGINT — marker missing\nparent stderr:\n%s", pStderr.String())
	}
	_ = cmd.Wait()

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
