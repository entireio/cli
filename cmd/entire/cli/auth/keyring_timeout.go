package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"
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

// newKeyringContext returns a context that cancels after the configured
// keyring timeout. Callers must invoke the returned cancel to release
// the timer regardless of which select branch wins.
func newKeyringContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), keyringTimeout())
}

// callKeyringWithContext runs fn in a goroutine and returns its result,
// or a descriptive error if ctx is cancelled (e.g. its deadline
// elapses) first. The goroutine continues running — a blocked D-Bus
// syscall can't be cancelled from Go — and its eventual result is
// discarded. The buffered result channel keeps the goroutine from
// leaking forever waiting to publish into a receiver that's already
// gone.
func callKeyringWithContext(ctx context.Context, op string, fn func() (string, error)) (string, error) {
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
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf(
				"%s timed out: OS keyring (%s) appears unavailable; set %s to a longer duration to wait further: %w",
				op, keyringProviderName(), keyringTimeoutEnvVar, ctx.Err(),
			)
		}
		return "", fmt.Errorf("%s cancelled: %w", op, ctx.Err())
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
