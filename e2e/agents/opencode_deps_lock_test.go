package agents

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestBuildPluginDepsWaitsForPublisher(t *testing.T) {
	t.Parallel()
	const helperDirEnv = "ENTIRE_TEST_OPENCODE_DEPS_LOCK_DIR"
	if dir := os.Getenv(helperDirEnv); dir != "" {
		fmt.Println("ready")
		got, err := buildPluginDepsAt("1.0.0", dir, func(string) error {
			return errors.New("waiter must reuse the publisher's completed tree")
		})
		if err != nil || got != dir {
			t.Fatalf("waiting builder = %q, %v", got, err)
		}
		return
	}

	dir := filepath.Join(t.TempDir(), "deps")
	writeOpenCodeDepsFixture(t, dir)
	if err := os.Remove(filepath.Join(dir, "package-lock.json")); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(dir + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()

	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestBuildPluginDepsWaitsForPublisher$")
	cmd.Env = append(os.Environ(), helperDirEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("waiting builder did not signal readiness: %v", scanner.Err())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("builder did not wait for the publisher's lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	// The incomplete cache must not be removed while another process owns it.
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		t.Errorf("waiting builder removed publisher's tree: %v", err)
	}
	writeOpenCodeDepsFixture(t, dir)
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("waiting builder failed after publication: %v", err)
	}
}

func TestInstallOpenCodePluginDepsForcesLockfile(t *testing.T) {
	// Not parallel: npm configuration is passed through the process environment.
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	dir := t.TempDir()
	t.Setenv("npm_config_cache", filepath.Join(dir, "cache"))
	t.Setenv("npm_config_userconfig", filepath.Join(dir, "user.npmrc"))
	t.Setenv("npm_config_globalconfig", filepath.Join(dir, "global.npmrc"))
	t.Setenv("npm_config_package_lock", "false")
	t.Setenv("npm_config_offline", "true")
	t.Setenv("npm_config_update_notifier", "false")
	// An empty package keeps this a real npm invocation without downloading
	// dependencies or running an agent. Isolate npm's cache and user config.
	files := map[string]string{
		"package.json": "{\"name\":\"cache-lockfile-test\",\"version\":\"1.0.0\",\"private\":true}\n",
		".npmrc":       "package-lock=false\n",
		"user.npmrc":   "",
		"global.npmrc": "",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := installOpenCodePluginDeps(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "package-lock.json")); err != nil {
		t.Fatalf("npm package-lock=false prevented lockfile generation: %v", err)
	}
}
