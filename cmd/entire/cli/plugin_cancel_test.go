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

// pretendTerminalSentInterrupt stands in for a terminal whose foreground
// process group holds this process and the plugin.
func pretendTerminalSentInterrupt(t *testing.T) {
	t.Helper()
	prev := pluginTerminalSentInterrupt
	pluginTerminalSentInterrupt = func(int) bool { return true }
	t.Cleanup(func() { pluginTerminalSentInterrupt = prev })
}

// A Ctrl-C the terminal delivered to the plugin is not sent again: a second
// copy would read as the user's second Ctrl-C and force-quit the plugin.
func TestRunPlugin_TerminalInterruptIsNotForwarded(t *testing.T) { //nolint:paralleltest // mutates env, seams and procsignal
	pretendTerminalSentInterrupt(t)
	storeReceivedSignal(t, os.Interrupt)
	// The trap tells the two apart: a forwarded SIGINT would exit 9.
	code, sig := startCancelledPlugin(t, "trap 'exit 9' INT\ntouch \"$READY\"\nsleep 0.5\nexit 3\n")
	if code != 3 || sig != nil {
		t.Errorf("runPlugin = (%d, %v), want the plugin's own (3, nil): the SIGINT was forwarded", code, sig)
	}
}

// shortenPluginCancelGrace sets pluginCancelGrace to d for the test.
func shortenPluginCancelGrace(t *testing.T, d time.Duration) {
	t.Helper()
	prev := pluginCancelGrace
	pluginCancelGrace = d
	t.Cleanup(func() { pluginCancelGrace = prev })
}

// A plugin the terminal sent the Ctrl-C to is not killed after
// pluginCancelGrace: it got the Ctrl-C itself and decides how long its
// wind-down takes (entire-stream closes its live session, which can outlast
// the grace). Its own exit status is what we report.
func TestRunPlugin_TerminalInterruptIsNotKilledAfterGrace(t *testing.T) { //nolint:paralleltest // mutates env, seams and procsignal
	pretendTerminalSentInterrupt(t)
	storeReceivedSignal(t, os.Interrupt)
	shortenPluginCancelGrace(t, 100*time.Millisecond)
	code, sig := startCancelledPlugin(t, "trap 'exit 9' INT\ntouch \"$READY\"\nsleep 1\nexit 3\n")
	if code != 3 || sig != nil {
		t.Errorf("runPlugin = (%d, %v), want the plugin's own (3, nil) after its wind-down", code, sig)
	}
}

// ...but a second Ctrl-C still ends it: ForceQuitPlugins kills it once
// pluginForceQuitGrace has passed, so the CLI never exits leaving it behind.
func TestRunPlugin_TerminalInterruptThenForceQuitKillsPlugin(t *testing.T) { //nolint:paralleltest // mutates env, seams and procsignal
	pretendTerminalSentInterrupt(t)
	storeReceivedSignal(t, os.Interrupt)
	shortenPluginCancelGrace(t, 100*time.Millisecond)
	prev := pluginForceQuitGrace
	pluginForceQuitGrace = 200 * time.Millisecond
	t.Cleanup(func() { pluginForceQuitGrace = prev })
	var stillRunning bool
	start := time.Now()
	code, sig := startPluginUntilReady(t, "trap '' INT\ntouch \"$READY\"\nexec sleep 30\n", func(cancel context.CancelFunc) {
		cancel()
		time.Sleep(3 * pluginCancelGrace)
		runningPlugins.Lock()
		stillRunning = len(runningPlugins.procs) == 1
		runningPlugins.Unlock()
		ForceQuitPlugins(os.Interrupt, nil)
	})
	if !stillRunning {
		t.Error("plugin was not running past pluginCancelGrace before the second Ctrl-C")
	}
	if code != ExitPluginSignalled || sig != syscall.SIGKILL {
		t.Errorf("runPlugin = (%d, %v), want (%d, killed)", code, sig, ExitPluginSignalled)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("plugin ran %v, want it killed by the force quit", elapsed)
	}
}

// A forwarded signal still comes with the kill after pluginCancelGrace: a
// SIGTERM (even with a terminal in the foreground), or a Ctrl-C that did not
// come from the terminal, reached this process alone, and a plugin that
// ignores it would otherwise outlive the CLI's shutdown.
func TestRunPlugin_ForwardedSignalKillsPluginAfterGrace(t *testing.T) { //nolint:paralleltest // mutates env, seams and procsignal
	for _, tc := range []struct {
		name     string
		received os.Signal
		terminal bool
	}{
		{name: "SIGTERM with a terminal", received: syscall.SIGTERM, terminal: true},
		{name: "Ctrl-C not from the terminal", received: os.Interrupt, terminal: false},
	} {
		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest // mutates env, seams and procsignal
			prev := pluginTerminalSentInterrupt
			pluginTerminalSentInterrupt = func(int) bool { return tc.terminal }
			t.Cleanup(func() { pluginTerminalSentInterrupt = prev })
			storeReceivedSignal(t, tc.received)
			shortenPluginCancelGrace(t, 200*time.Millisecond)
			start := time.Now()
			code, sig := startCancelledPlugin(t, "trap '' INT TERM\ntouch \"$READY\"\nexec sleep 30\n")
			if code != ExitPluginSignalled || sig != syscall.SIGKILL {
				t.Errorf("runPlugin = (%d, %v), want (%d, killed)", code, sig, ExitPluginSignalled)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Errorf("plugin ran %v after cancel, want it killed after the grace period", elapsed)
			}
		})
	}
}

// Only a Ctrl-C is the terminal's: a SIGTERM reached this process alone and
// is still forwarded.
func TestRunPlugin_TerminalCheckLeavesSIGTERMForwarded(t *testing.T) { //nolint:paralleltest // mutates env, seams and procsignal
	pretendTerminalSentInterrupt(t)
	storeReceivedSignal(t, syscall.SIGTERM)
	code, sig := startCancelledPlugin(t, "trap 'exit 7' TERM\ntouch \"$READY\"\nwhile :; do sleep 0.05; done\n")
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
		ForceQuitPlugins(syscall.SIGTERM, nil)
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
		ForceQuitPlugins(os.Interrupt, nil)
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
		ForceQuitPlugins(os.Interrupt, nil)
	})
	if code != ExitPluginSignalled || sig != syscall.SIGKILL {
		t.Errorf("runPlugin = (%d, %v), want (%d, killed)", code, sig, ExitPluginSignalled)
	}
}

// A third Ctrl-C during that grace kills at once rather than waiting it out.
func TestForceQuitPlugins_FurtherSignalCutsGraceShort(t *testing.T) { //nolint:paralleltest // mutates env and seams
	var took time.Duration
	code, sig := startPluginUntilReady(t, "trap '' INT\ntouch \"$READY\"\nexec sleep 30\n", func(context.CancelFunc) {
		more := make(chan os.Signal, 1)
		more <- os.Interrupt
		start := time.Now()
		ForceQuitPlugins(os.Interrupt, more)
		took = time.Since(start)
	})
	if code != ExitPluginSignalled || sig != syscall.SIGKILL {
		t.Errorf("runPlugin = (%d, %v), want (%d, killed)", code, sig, ExitPluginSignalled)
	}
	if took > time.Second {
		t.Errorf("ForceQuitPlugins with a further signal took %v, want an immediate kill", took)
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

	// Unix kills from Cancel's own timer, and only after forwarding a signal:
	// exec's WaitDelay would also kill a plugin the terminal sent Ctrl-C to.
	cmd = exec.CommandContext(context.Background(), "unused")
	configurePluginCancel(cmd, "linux")
	if cmd.WaitDelay != 0 {
		t.Errorf("Unix WaitDelay = %v, want 0", cmd.WaitDelay)
	}
}
