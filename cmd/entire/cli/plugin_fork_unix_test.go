//go:build !windows

package cli

import "syscall"

// pauseForks blocks every fork/exec this process starts until the returned
// resume is called.
//
// syscall.forkExec takes ForkLock for WRITING around the fork, precisely so
// that a descriptor created while ForkLock is held for READING cannot be alive
// in the fd table a fork copies. That is the guarantee writeExecutableScript
// needs, and taking the read lock is the documented way to ask for it — see
// the ForkLock comment in the standard library's syscall/exec_unix.go.
func pauseForks() (resume func()) {
	syscall.ForkLock.RLock()
	return syscall.ForkLock.RUnlock
}
