//go:build !windows

package codex

import "syscall"

// A regular-file replacement by a FIFO must not block before fstat can reject it.
const rolloutNonblock = syscall.O_NONBLOCK
