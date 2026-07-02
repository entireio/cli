// Package debuglog is the git-remote-entire helper's shared debug logger.
//
// Output is gated on ENTIRE_DEBUG: when unset, Printf is a no-op (and
// Enabled returns false so callers can skip work that only matters for a
// debug trace, e.g. dumping HTTP request bodies).
//
// The prefix is fixed because the binary that uses these internals is also
// fixed — the package lives under internal/, so its callers are known.
package debuglog

import (
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	envVar = "ENTIRE_DEBUG"
	prefix = "[git-remote-entire]"
)

// outMu protects out. A mutex (over atomic.Value) is fine here because
// debug logging is gated by ENTIRE_DEBUG — production performance never
// touches it, and tests serialize their own SetOutput swaps.
var (
	outMu sync.RWMutex
	out   io.Writer = os.Stderr
)

// Enabled reports whether debug output is on. Callers should gate any
// expensive preparation (request dumps, body buffering) behind this.
func Enabled() bool { return os.Getenv(envVar) != "" }

// Printf writes a line to stderr (or the configured sink) when Enabled,
// prefixed with [git-remote-entire]. A trailing newline is appended.
func Printf(format string, args ...any) {
	if !Enabled() {
		return
	}
	outMu.RLock()
	w := out
	outMu.RUnlock()
	if w == nil {
		return
	}
	fmt.Fprintf(w, prefix+" "+format+"\n", args...)
}

// SetOutput redirects debug output. Returns the previous writer so tests
// can restore it. Pass nil to discard.
func SetOutput(w io.Writer) io.Writer {
	if w == nil {
		w = io.Discard
	}
	outMu.Lock()
	prev := out
	out = w
	outMu.Unlock()
	return prev
}
