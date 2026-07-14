// Package procsignal records the OS signal, if any, that initiated process
// shutdown. It gives the CLI a single source of truth for "were we signalled?"
// shared by the two places that can observe a terminating signal:
//
//   - the top-level signal handler in cmd/entire, which cancels the root
//     context on SIGINT/SIGTERM, and
//   - the keyring interrupt path in internal/entireclient/tokenstore, which
//     detects Ctrl-C via its own signal.Notify listener so a stuck keyring
//     read unblocks immediately.
//
// Before this package the two mechanisms were uncoordinated: the keyring path
// returned a context.Canceled error while the top-level handler stored the
// caught signal asynchronously on a different goroutine, so the CLI's
// signal-abort gate could read the store before it was set and misreport a
// user abort as a failure (and fail to break an enclosing shell loop).
// Recording the signal here — on the same goroutine that unwinds to the gate —
// removes that race.
//
// Tech debt: this shared global exists only because auth-go's Store interface
// (LoadTokens/SaveTokens) carries no context.Context, so the keyring path can't
// ride the root context and instead detects Ctrl-C via its own signal listener.
// Once that interface accepts a context, keyring cancellation can flow from the
// root context like everything else, the tokenstore signal listener can go
// away, and this package with it.
package procsignal

import (
	"os"
	"sync/atomic"
)

// holder wraps the stored signal so atomic.Value always observes one concrete
// type. Storing differing concrete types (or nil) into an atomic.Value panics;
// wrapping avoids both.
type holder struct{ sig os.Signal }

var caught atomic.Value // stores holder

// Store records sig as the signal that initiated shutdown. Safe for concurrent
// use; last writer wins, which is fine because every caller stores a genuine
// terminating signal.
func Store(sig os.Signal) {
	caught.Store(holder{sig: sig})
}

// Load returns the recorded terminating signal, or nil if none was recorded.
func Load() os.Signal {
	if h, ok := caught.Load().(holder); ok {
		return h.sig
	}
	return nil
}

// Reset clears the recorded signal. It exists for tests that need a clean
// slate; production code never clears it (the process is on its way out).
func Reset() {
	caught.Store(holder{})
}
