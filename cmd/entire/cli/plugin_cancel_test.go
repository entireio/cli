package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/internal/procsignal"
)

// startCancelledPlugin runs script as a plugin, cancels the context once the
// script has created its ready marker, and returns runPlugin's outcome.
func startCancelledPlugin(t *testing.T, script string) (int, os.Signal) {
	t.Helper()
	return startPluginUntilReady(t, script, func(cancel context.CancelFunc) { cancel() })
}

// startPluginUntilReady runs script as a plugin, calls onReady once the script
// has created its ready marker (or after 10s), and returns runPlugin's outcome.
func startPluginUntilReady(t *testing.T, script string, onReady func(cancel context.CancelFunc)) (int, os.Signal) {
	t.Helper()
	if runtime.GOOS == windowsGOOS {
		t.Skip("shell-script plugin harness only runs on Unix")
	}
	withPluginDir(t)
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	bin := filepath.Join(dir, "entire-canceller")
	writeExecutableScript(t, bin, "#!/bin/sh\nREADY='"+ready+"'\n"+script)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Joined before returning, so onReady never outlives the test's seams.
	done := make(chan struct{})
	defer func() { <-done }()
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		onReady(cancel)
	}()
	return runPlugin(ctx, "canceller", bin, nil)
}

// useWindowsPluginCancel gives runPlugin the Windows cancel behaviour for the
// test, so it runs against real child processes on any platform.
func useWindowsPluginCancel(t *testing.T) {
	t.Helper()
	prev := pluginCancelGOOS
	pluginCancelGOOS = windowsGOOS
	t.Cleanup(func() { pluginCancelGOOS = prev })
}

// A plugin that handles the Ctrl-C itself and exits 0 keeps its exit code.
// exec reports the cancellation (context.Canceled) instead of the exit, which
// used to surface as "Failed to run plugin ...: context canceled" and exit 1.
func TestRunPlugin_CancelledPluginThatExitsCleanlyKeepsExitZero(t *testing.T) { //nolint:paralleltest // mutates env and seams
	code, sig := startCancelledPlugin(t, "trap 'exit 0' INT\ntouch \"$READY\"\nwhile :; do sleep 0.05; done\n")
	if code != 0 || sig != nil {
		t.Errorf("runPlugin = (%d, %v), want (0, nil)", code, sig)
	}
}

// A plugin without a SIGINT handler still dies from the SIGINT we forward,
// and that stays reported as a signal.
func TestRunPlugin_CancelledPluginKilledBySIGINTReportsSignal(t *testing.T) { //nolint:paralleltest // mutates env and seams
	// exec: a shell waiting on a foreground child defers SIGINT until the
	// child exits, so the signal must reach a process with default handling.
	code, sig := startCancelledPlugin(t, "touch \"$READY\"\nexec sleep 30\n")
	if code != ExitPluginSignalled || sig != os.Interrupt {
		t.Errorf("runPlugin = (%d, %v), want (%d, interrupt)", code, sig, ExitPluginSignalled)
	}
}

// The Windows configuration, run against a real child: cancelling neither
// signals nor kills the plugin (the console already delivered Ctrl-C to it),
// so it finishes its wind-down and its exit code is what we report.
func TestRunPlugin_WindowsCancelWaitsForPluginToExit(t *testing.T) { //nolint:paralleltest // mutates env and seams
	useWindowsPluginCancel(t)
	if runtime.GOOS == windowsGOOS {
		t.Skip("covered on Windows by TestRunPlugin_WindowsCancelWaitsForRealPlugin")
	}
	// The trap tells the two apart: a forwarded SIGINT would exit 9.
	code, sig := startCancelledPlugin(t, "trap 'exit 9' INT\ntouch \"$READY\"\nsleep 0.5\nexit 3\n")
	if code != 3 || sig != nil {
		t.Errorf("runPlugin = (%d, %v), want the plugin's own (3, nil)", code, sig)
	}
}

// The Windows configuration still kills a plugin that ignores the Ctrl-C,
// once pluginWindowsCancelGrace has passed: nothing else would end it.
func TestRunPlugin_WindowsCancelKillsPluginAfterGrace(t *testing.T) { //nolint:paralleltest // mutates env and seams
	useWindowsPluginCancel(t)
	if runtime.GOOS == windowsGOOS {
		t.Skip("shell-script plugin harness only runs on Unix")
	}
	prev := pluginWindowsCancelGrace
	pluginWindowsCancelGrace = 200 * time.Millisecond
	t.Cleanup(func() { pluginWindowsCancelGrace = prev })
	start := time.Now()
	code, sig := startCancelledPlugin(t, "trap '' INT\ntouch \"$READY\"\nexec sleep 30\n")
	if code != ExitPluginSignalled || sig != syscall.SIGKILL {
		t.Errorf("runPlugin = (%d, %v), want (%d, killed)", code, sig, ExitPluginSignalled)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("plugin ran %v after cancel, want it killed after the grace period", elapsed)
	}
}

// storeReceivedSignal records sig as the signal this process received, as
// main's handler does, for the duration of the test.
func storeReceivedSignal(t *testing.T, sig os.Signal) {
	t.Helper()
	procsignal.Store(sig)
	t.Cleanup(procsignal.Reset)
}

// A SIGTERM we receive reaches the plugin as SIGTERM, not as a Ctrl-C: the
// plugin can tell a supervisor's shutdown from a user's interrupt.
func TestRunPlugin_CancelForwardsTheReceivedSignal(t *testing.T) { //nolint:paralleltest // mutates env, seams and procsignal
	storeReceivedSignal(t, syscall.SIGTERM)
	code, sig := startCancelledPlugin(t, "trap 'exit 7' TERM\ntrap 'exit 9' INT\ntouch \"$READY\"\nwhile :; do sleep 0.05; done\n")
	if code != 7 || sig != nil {
		t.Errorf("runPlugin = (%d, %v), want (7, nil) from the plugin's TERM trap", code, sig)
	}
}

// A force quit by anything but Ctrl-C reached only us, so a plugin is killed
// at once rather than left running after we exit.
func TestForceQuitPlugins_NonInterruptKillsAtOnce(t *testing.T) { //nolint:paralleltest // mutates env and seams
	var took time.Duration
	code, sig := startPluginUntilReady(t, "trap '' INT TERM HUP\ntouch \"$READY\"\nexec sleep 30\n", func(context.CancelFunc) {
		start := time.Now()
		ForceQuitPlugins(syscall.SIGTERM)
		took = time.Since(start)
	})
	if code != ExitPluginSignalled || sig != syscall.SIGKILL {
		t.Errorf("runPlugin = (%d, %v), want (%d, killed)", code, sig, ExitPluginSignalled)
	}
	if took > time.Second {
		t.Errorf("ForceQuitPlugins(SIGTERM) took %v, want an immediate kill", took)
	}
	assertNoTrackedPlugins(t)
}

// A second Ctrl-C reached the plugin too, so it gets to finish its own force
// quit (entire-stream flushes its last records) instead of being killed.
func TestForceQuitPlugins_InterruptLetsPluginFinish(t *testing.T) { //nolint:paralleltest // mutates env and seams
	code, sig := startPluginUntilReady(t, "touch \"$READY\"\nsleep 0.5\nexit 3\n", func(context.CancelFunc) {
		ForceQuitPlugins(os.Interrupt)
	})
	if code != 3 || sig != nil {
		t.Errorf("runPlugin = (%d, %v), want the plugin's own (3, nil)", code, sig)
	}
}

// ...but only for pluginForceQuitGrace: a plugin still running after that is
// killed, so the CLI never exits leaving it behind.
func TestForceQuitPlugins_InterruptKillsAfterGrace(t *testing.T) { //nolint:paralleltest // mutates env and seams
	prev := pluginForceQuitGrace
	pluginForceQuitGrace = 200 * time.Millisecond
	t.Cleanup(func() { pluginForceQuitGrace = prev })
	code, sig := startPluginUntilReady(t, "trap '' INT\ntouch \"$READY\"\nexec sleep 30\n", func(context.CancelFunc) {
		ForceQuitPlugins(os.Interrupt)
	})
	if code != ExitPluginSignalled || sig != syscall.SIGKILL {
		t.Errorf("runPlugin = (%d, %v), want (%d, killed)", code, sig, ExitPluginSignalled)
	}
}

func assertNoTrackedPlugins(t *testing.T) {
	t.Helper()
	runningPlugins.Lock()
	n := len(runningPlugins.procs)
	runningPlugins.Unlock()
	if n != 0 {
		t.Errorf("%d plugin processes still tracked after runPlugin returned", n)
	}
}

func TestConfigurePluginCancel_Windows(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), "unused")
	configurePluginCancel(cmd, windowsGOOS)
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Windows Cancel = %v, want nil (Ctrl-C already reached the plugin)", err)
	}
	if cmd.WaitDelay != pluginWindowsCancelGrace {
		t.Errorf("Windows WaitDelay = %v, want %v", cmd.WaitDelay, pluginWindowsCancelGrace)
	}

	cmd = exec.CommandContext(context.Background(), "unused")
	configurePluginCancel(cmd, "linux")
	if cmd.WaitDelay != pluginCancelGrace {
		t.Errorf("Unix WaitDelay = %v, want %v", cmd.WaitDelay, pluginCancelGrace)
	}
}
