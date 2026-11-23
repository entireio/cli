//go:build windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// On real Windows, cancelling the context used to make exec call
// Process.Signal(os.Interrupt), which Windows does not support: runPlugin
// printed "exec: canceling Cmd: not supported by windows", returned 1, and
// killed the plugin 5s later. Now the plugin runs to its own exit.
func TestRunPlugin_WindowsCancelWaitsForRealPlugin(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	// ping.exe -n 3 runs ~2s and exits 0: a stand-in for a plugin winding
	// down after Ctrl-C.
	ping := filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE")
	if _, err := os.Stat(ping); err != nil {
		t.Skipf("no ping.exe: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)

	start := time.Now()
	code, sig := runPlugin(ctx, "ping", ping, []string{"-n", "3", "127.0.0.1"})
	if code != 0 || sig != nil {
		t.Errorf("runPlugin = (%d, %v), want (0, nil)", code, sig)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("runPlugin returned after %v; the plugin was cut short instead of awaited", elapsed)
	}
}
