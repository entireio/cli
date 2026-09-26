//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// pluginTerminatingSignal reports the signal a plugin was killed by, or nil
// when it exited normally.
//
// Build-tagged rather than switched on runtime.GOOS because the API is absent
// on Windows, not merely inapplicable: syscall.WaitStatus there is a bare
// struct with an ExitCode field and no Signaled/Signal methods, so a
// runtime.GOOS branch would not compile.
//
// A signal is not something the parent necessarily saw. Ctrl-C reaches the
// whole foreground process group, but `kill -TERM` aimed at the plugin, and a
// SIGPIPE from `entire graph | head -1`, reach the child alone — and
// kubectl-style dispatch has to propagate the external command's outcome
// either way, so the signal has to come off the child's wait status rather
// than out of what this process was told.
func pluginTerminatingSignal(state *os.ProcessState) os.Signal {
	if state == nil {
		return nil
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return nil
	}
	return ws.Signal()
}
