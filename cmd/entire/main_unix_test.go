//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
