//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// `nohup entire <plugin> &`, then the terminal closes: both entire and the
// plugin must sleep through the SIGHUP, as nohup intends. Catching it would
// cancel the plugin and kill it after the grace period.
func TestNohup_EntireSleepsThroughSIGHUP(t *testing.T) {
	t.Parallel()
	nohup, err := exec.LookPath("nohup")
	if err != nil {
		t.Skipf("no nohup: %v", err)
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "plugin.pid")
	plugin := filepath.Join(binDir, "entire-hupper")
	script := "#!/bin/sh\necho $$ > \"$ENTIRE_TEST_HUP_PIDFILE\"\nexec sleep 30\n"
	if err := os.WriteFile(plugin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	entire := exec.CommandContext(t.Context(), nohup, os.Args[0], "-test.run=^$")
	entire.Dir = dir
	entire.Env = append(os.Environ(),
		runMainEnvVar+"=hupper",
		"ENTIRE_TEST_HUP_PIDFILE="+pidFile,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ENTIRE_CONFIG_DIR="+filepath.Join(dir, "config"),
		"ENTIRE_PLUGIN_DIR="+filepath.Join(dir, "plugins"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"ENTIRE_TOKEN_STORE=file",
		"ENTIRE_TOKEN_STORE_PATH="+filepath.Join(dir, "tokens.json"),
		"ENTIRE_TEST_AUTH_STORE_FILE="+filepath.Join(dir, "auth.json"),
		"ENTIRE_TELEMETRY_OPTOUT=1",
	)
	var stderr strings.Builder
	entire.Stderr = &stderr
	if err := entire.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = entire.Wait(); close(exited) }()
	pluginPID := 0
	t.Cleanup(func() {
		if pluginPID != 0 {
			_ = syscall.Kill(pluginPID, syscall.SIGKILL) //nolint:errcheck // test cleanup
		}
		_ = entire.Process.Kill() //nolint:errcheck // test cleanup
		<-exited
	})

	deadline := time.Now().Add(15 * time.Second)
	for pluginPID == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			pluginPID, _ = strconv.Atoi(strings.TrimSpace(string(data))) //nolint:errcheck // 0 retries
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pluginPID == 0 {
		t.Fatalf("plugin never started; entire stderr: %s", stderr.String())
	}

	// SIGHUP, then SIGTERM. No sleep is needed to tell the outcomes apart:
	// the kernel discards an ignored SIGHUP, so SIGTERM is the first signal
	// entire sees, it forwards it, the plugin dies, and entire dies by
	// SIGTERM. Had entire caught the SIGHUP, that would be its first signal
	// (the lower-numbered of two pending signals is delivered first) and it
	// would die by SIGHUP.
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		if err := entire.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("entire still running 30s after SIGTERM; stderr: %s", stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("entire exited with %v, want death by SIGTERM; stderr: %s", waitErr, stderr.String())
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("entire ended with %v, want death by SIGTERM: a SIGHUP death means it caught the SIGHUP nohup ignored; stderr: %s", exitErr, stderr.String())
	}
}

// One Ctrl-C typed in the terminal reaches a plugin as one SIGINT, and the
// plugin's clean exit is entire's. The terminal delivers it to the whole
// foreground process group, plugin included; forwarding entire's copy as well
// would make one key press look like the second Ctrl-C a plugin treats as
// force quit. This test cannot reliably see that duplicate: it lands
// microseconds after the original, and Go's os/signal keeps one pending bit
// per signal, so this Go plugin usually counts the pair as one (seen on macOS
// and Linux). TestTerminalSentInterrupt_UnderATerminal guards the detection.
func TestTerminalCtrlC_ReachesPluginOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.Args[0], filepath.Join(binDir, "entire-sigcount")); err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(dir, "count")

	entire := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	entire.Dir = dir
	entire.Env = append(os.Environ(),
		runMainEnvVar+"=sigcount",
		sigCountEnvVar+"="+countFile,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ENTIRE_CONFIG_DIR="+filepath.Join(dir, "config"),
		"ENTIRE_PLUGIN_DIR="+filepath.Join(dir, "plugins"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"ENTIRE_TOKEN_STORE=file",
		"ENTIRE_TOKEN_STORE_PATH="+filepath.Join(dir, "tokens.json"),
		"ENTIRE_TEST_AUTH_STORE_FILE="+filepath.Join(dir, "auth.json"),
		"ENTIRE_TELEMETRY_OPTOUT=1",
	)
	// Its own session with the pty as controlling terminal, so entire's
	// process group is the terminal's foreground group, as in a shell.
	terminal, err := pty.StartWithAttrs(entire, &pty.Winsize{Rows: 24, Cols: 100}, &syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	go func() { _, _ = io.Copy(io.Discard, terminal) }() //nolint:errcheck // ends with EIO when entire exits
	exited := make(chan error, 1)
	go func() { exited <- entire.Wait() }()
	t.Cleanup(func() { _ = entire.Process.Kill() }) //nolint:errcheck // test cleanup

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(countFile + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("plugin never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := terminal.Write([]byte{0x03}); err != nil { // Ctrl-C
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("entire exited with %v, want the plugin's 0", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("entire still running 30s after Ctrl-C")
	}
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("plugin wrote no count: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Errorf("plugin received %s SIGINTs for one Ctrl-C, want 1", got)
	}
}
