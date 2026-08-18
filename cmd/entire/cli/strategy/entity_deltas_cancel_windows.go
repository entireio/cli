//go:build windows

package strategy

import "os/exec"

// killEntityDeltasProcessGroupOnCancel is a no-op on Windows: reliable tree-kill
// needs a Job Object. The WaitDelay backstop still bounds the wait on a hung
// producer. Mirrors checkpoint/remote's killProcessGroupOnCancel.
func killEntityDeltasProcessGroupOnCancel(_ *exec.Cmd) {}
