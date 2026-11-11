//go:build unix

package main

import (
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
func TestNohup_SIGHUPLeavesEntireAndPluginRunning(t *testing.T) {
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
	go func() { _ = entire.Wait(); close(exited) }() //nolint:errcheck // killed at cleanup
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

	if err := entire.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	select {
	case <-exited:
		t.Fatalf("entire exited after a SIGHUP it was started ignoring; stderr: %s", stderr.String())
	default:
	}
	if err := syscall.Kill(pluginPID, 0); err != nil {
		t.Errorf("plugin gone after a SIGHUP nohup ignored: %v", err)
	}
	if strings.Contains(stderr.String(), "shutting down") {
		t.Errorf("entire caught the SIGHUP nohup ignored; stderr: %s", stderr.String())
	}
}
