// Package flock provides a small cross-process advisory-lock primitive built
// on POSIX flock (Unix) / LockFileEx (Windows). It exists so that checkpoint
// and strategy can both serialize on shared resources without one taking
// the other as an import dependency.
package flock

import (
	"sync"
	"time"
)

// acquirePollInterval is how often AcquireContext retries a non-blocking lock
// attempt while waiting for a contended lock to free up. Shared by the Unix and
// Windows implementations.
const acquirePollInterval = 10 * time.Millisecond

// onceRelease wraps a release function so it runs at most once. Callers are
// documented to release exactly once, but a double release would (on Windows)
// call UnlockFileEx on an already-closed handle; sync.Once makes it a no-op.
func onceRelease(release func()) func() {
	var once sync.Once
	return func() { once.Do(release) }
}
