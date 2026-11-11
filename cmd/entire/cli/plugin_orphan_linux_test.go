//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A SIGKILLed CLI takes its plugin with it: Pdeathsig, where no handler of
// ours gets to run.
func TestRunPlugin_PluginDiesWithKilledCLI(t *testing.T) {
	t.Parallel()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	helper, pid := startOrphanHelper(t, sleep, "60")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) }) //nolint:errcheck // only if the test failed

	if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait() //nolint:errcheck // killed on purpose

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if linuxProcessGone(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("plugin %d still running 10s after its CLI was SIGKILLed", pid)
}

// linuxProcessGone reports whether pid has exited: no such process, or a
// zombie its new parent has yet to reap.
func linuxProcessGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	// "pid (comm) S ...": the state follows the last ')'.
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}
