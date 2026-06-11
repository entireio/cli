//go:build windows

package remote

import "os/exec"

// killProcessGroupOnCancel is a no-op on Windows: reliable tree-kill needs a Job
// Object. The WaitDelay backstop still bounds the wait on a hung subprocess.
func killProcessGroupOnCancel(_ *exec.Cmd) {}
