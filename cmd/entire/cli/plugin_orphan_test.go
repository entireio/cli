//go:build linux || windows

// Shared by the platform tests that a killed CLI takes its plugin with it;
// macOS has no mechanism for that (see plugin_process_other.go).

package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// orphanHelperEnv carries the helper's job to the re-executed test binary:
// JSON of orphanHelperJob.
const orphanHelperEnv = "ENTIRE_TEST_PLUGIN_ORPHAN_HELPER"

type orphanHelperJob struct {
	Bin     string   `json:"bin"`
	Args    []string `json:"args"`
	PIDFile string   `json:"pid_file"`
}

// TestPluginOrphanHelper is not a test: it is the process startOrphanHelper
// launches. It runs a plugin through runPlugin, writes the plugin's PID, and
// waits to be killed.
func TestPluginOrphanHelper(t *testing.T) { //nolint:paralleltest // a helper process, never run in-suite
	raw := os.Getenv(orphanHelperEnv)
	if raw == "" {
		t.Skip("helper process for the plugin orphan tests")
	}
	var job orphanHelperJob
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatal(err)
	}
	go runPlugin(context.Background(), "orphan", job.Bin, job.Args)
	for {
		runningPlugins.Lock()
		var pid int
		for proc := range runningPlugins.procs {
			pid = proc.Pid
		}
		runningPlugins.Unlock()
		if pid != 0 {
			if err := os.WriteFile(job.PIDFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
				t.Fatal(err)
			}
			select {} // until killed
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startOrphanHelper starts a helper process running bin as a plugin and
// returns the helper and the plugin's PID.
func startOrphanHelper(t *testing.T, bin string, args ...string) (*exec.Cmd, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "plugin.pid")
	job, err := json.Marshal(orphanHelperJob{Bin: bin, Args: args, PIDFile: pidFile})
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestPluginOrphanHelper$")
	helper.Env = append(os.Environ(), orphanHelperEnv+"="+string(job))
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill() //nolint:errcheck // may already be dead
		_ = helper.Wait()         //nolint:errcheck // killed on purpose
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil && len(data) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			return helper, pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper never reported its plugin's PID")
	return nil, 0
}
