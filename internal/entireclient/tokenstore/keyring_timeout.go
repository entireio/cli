package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/entireio/cli/internal/procsignal"
)

// defaultKeyringTimeout caps how long every OS keyring call may take.
// The underlying keyring API (Secret Service on Linux, Keychain on
// macOS, Credential Manager on Windows) can block indefinitely when no
// provider is reachable — a headless SSH/container/WSL session, a
// suppressed Keychain prompt, a stuck Credential Manager — and that
// freezes the CLI. 5s is comfortably longer than any healthy
// round-trip while still surfacing the hang to the user quickly.
const defaultKeyringTimeout = 5 * time.Second

// keyringTimeoutEnvVar overrides defaultKeyringTimeout. Accepts any
// time.ParseDuration string; invalid or non-positive values fall back
// to the default. Useful on slow keyrings or to extend the wait on a
// system where the secret service is just sluggish.
const keyringTimeoutEnvVar = "ENTIRE_KEYRING_TIMEOUT"

func keyringTimeout() time.Duration {
	v := os.Getenv(keyringTimeoutEnvVar)
	if v == "" {
		return defaultKeyringTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultKeyringTimeout
	}
	return d
}

// callKeyringWithTimeout runs fn in a goroutine and returns its result,
// or a descriptive error if the configured keyring timeout elapses or the
// user interrupts (Ctrl-C) first. The goroutine continues running — a
// blocked D-Bus syscall / Keychain subprocess can't be cancelled from Go —
// and its eventual result is discarded. The buffered result channel keeps
// the goroutine from leaking forever waiting to publish into a receiver
// that's already gone. fn's own error (including ErrNotFound) propagates
// unchanged on the fast path; only the timeout and interrupt branches wrap.
//
// It listens for SIGINT for the duration of the call so a Ctrl-C unblocks a
// stuck keyring read *now* rather than after the full timeout. This is the
// only cancellation lever available here: the credential store is reached
// through auth-go's Store interface (LoadTokens/SaveTokens), which carries
// no context.Context, so a per-request context can't be threaded down to
// this point. signal.Notify fans a signal out to every registered channel,
// so the process's own handler (which cancels the root context) still runs
// — this is an additional listener scoped to the keyring call.
func callKeyringWithTimeout(op string, fn func() (string, error)) (string, error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	return recordInterruptSignal(callKeyringWithInterrupt(op, keyringTimeout(), fn, sigCh))
}

// recordInterruptSignal records the shared "we were signalled" marker when the
// keyring call was aborted by a Ctrl-C (a wrapped context.Canceled from the
// interrupt branch below). It runs on the goroutine that unwinds to the CLI's
// top-level signal-abort gate, so the store is ordered before that gate reads
// procsignal — closing the race against the asynchronous signal handler that
// also received the SIGINT. A timeout wraps context.DeadlineExceeded, not
// Canceled, so it is left untouched.
func recordInterruptSignal(val string, err error) (string, error) {
	if errors.Is(err, context.Canceled) {
		procsignal.Store(os.Interrupt)
	}
	return val, err
}

// callKeyringWithInterrupt is the testable core of callKeyringWithTimeout:
// the interrupt source is injected so tests can exercise the Ctrl-C branch
// without sending real signals to the test process.
func callKeyringWithInterrupt(op string, timeout time.Duration, fn func() (string, error), interrupt <-chan os.Signal) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	type result struct {
		val string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{val: v, err: err}
	}()
	select {
	case r := <-ch:
		return r.val, r.err
	case <-interrupt:
		// Wrap context.Canceled so the abort flows into the CLI's silent
		// "user aborted" exit path rather than printing as a keyring failure.
		return "", fmt.Errorf("%s interrupted: %w", op, context.Canceled)
	case <-ctx.Done():
		return "", fmt.Errorf(
			"%s timed out: OS keyring (%s) appears unavailable; set %s to a longer duration to wait further: %w",
			op, keyringProviderName(), keyringTimeoutEnvVar, ctx.Err(),
		)
	}
}

// keyringProviderName returns the human name of the OS keyring backend
// for the current platform, so the timeout error can point the user at
// the specific service that's likely stuck (Keychain on macOS,
// Credential Manager on Windows, Secret Service on Linux/BSD). The
// fallback for unrecognised GOOS is the generic "OS keyring".
func keyringProviderName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS Keychain"
	case "windows":
		return "Windows Credential Manager"
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		return "Secret Service (D-Bus)"
	default:
		return "OS keyring"
	}
}
