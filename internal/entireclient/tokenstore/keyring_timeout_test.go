package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/internal/procsignal"
)

// notReturnedSentinel is the value fn returns when the test expects the
// interrupt/timeout branch to win the select, so this value must never surface.
const notReturnedSentinel = "should not be returned"

func TestCallKeyringWithTimeout_ReturnsValueWhenFast(t *testing.T) {
	t.Parallel()

	got, err := callKeyringWithTimeout("get", func() (string, error) {
		return "token", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "token" {
		t.Fatalf("got = %q, want %q", got, "token")
	}
}

func TestCallKeyringWithTimeout_PropagatesInnerError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("backend exploded")
	_, err := callKeyringWithTimeout("get", func() (string, error) {
		return "", sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want %v wrapped", err, sentinel)
	}
}

// A missing-credential error must reach the caller unchanged so the
// errors.Is(err, ErrNotFound) checks scattered through the auth package
// keep working through the timeout wrapper.
func TestCallKeyringWithTimeout_PropagatesNotFound(t *testing.T) {
	t.Parallel()

	_, err := callKeyringWithTimeout("get", func() (string, error) {
		return "", ErrNotFound
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCallKeyringWithTimeout_DeadlineExceeded(t *testing.T) {
	t.Setenv(keyringTimeoutEnvVar, "50ms")

	start := time.Now()
	_, err := callKeyringWithTimeout("get", func() (string, error) {
		time.Sleep(5 * time.Second)
		return notReturnedSentinel, nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded wrapped, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("call did not return promptly after timeout: elapsed=%s", elapsed)
	}

	msg := err.Error()
	for _, want := range []string{"get", "OS keyring", keyringTimeoutEnvVar} {
		if !strings.Contains(msg, want) {
			t.Errorf("timeout error %q missing %q", msg, want)
		}
	}
}

// A Ctrl-C must unblock a stuck keyring call immediately — well before the
// timeout — and surface as a context.Canceled so the CLI treats it as a user
// abort rather than a keyring failure.
func TestCallKeyringWithInterrupt_AbortsOnSignal(t *testing.T) {
	t.Parallel()

	interrupt := make(chan os.Signal, 1)
	started := make(chan struct{})
	start := time.Now()
	go func() {
		<-started
		interrupt <- os.Interrupt
	}()

	_, err := callKeyringWithInterrupt("get", 10*time.Second, func() (string, error) {
		close(started)
		time.Sleep(10 * time.Second) // never completes within the test
		return notReturnedSentinel, nil
	}, interrupt)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled wrapped, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("interrupt did not return promptly: elapsed=%s", elapsed)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error %q should mention it was interrupted", err.Error())
	}
}

// recordInterruptSignal must record a shared SIGINT marker for a Ctrl-C abort
// (wrapped context.Canceled) so the CLI's signal-abort gate recognizes it
// without racing the async top-level handler — but must leave the marker
// untouched for a timeout or any non-abort error. This test mutates the
// process-global procsignal state, so it can't run in parallel.
func TestRecordInterruptSignal(t *testing.T) {
	t.Run("records SIGINT on interrupt abort", func(t *testing.T) {
		procsignal.Reset()
		t.Cleanup(procsignal.Reset)

		val, err := recordInterruptSignal(callKeyringWithInterruptResult())
		if val != "" || !errors.Is(err, context.Canceled) {
			t.Fatalf("passthrough changed value/err: val=%q err=%v", val, err)
		}
		if got := procsignal.Load(); got != os.Interrupt {
			t.Fatalf("procsignal.Load() = %v, want SIGINT", got)
		}
	})

	t.Run("leaves marker unset on timeout", func(t *testing.T) {
		procsignal.Reset()
		t.Cleanup(procsignal.Reset)

		timeoutErr := fmt.Errorf("get timed out: %w", context.DeadlineExceeded)
		if _, err := recordInterruptSignal("", timeoutErr); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("passthrough changed err: %v", err)
		}
		if got := procsignal.Load(); got != nil {
			t.Fatalf("procsignal.Load() = %v, want nil (timeout is not a signal abort)", got)
		}
	})

	t.Run("leaves marker unset on success", func(t *testing.T) {
		procsignal.Reset()
		t.Cleanup(procsignal.Reset)

		if _, err := recordInterruptSignal("token", nil); err != nil {
			t.Fatalf("passthrough changed err: %v", err)
		}
		if got := procsignal.Load(); got != nil {
			t.Fatalf("procsignal.Load() = %v, want nil", got)
		}
	})
}

// callKeyringWithInterruptResult produces the exact (val, err) shape the
// interrupt branch returns, so the test exercises recordInterruptSignal against
// the real wrapped error rather than a hand-rolled one.
func callKeyringWithInterruptResult() (string, error) {
	interrupt := make(chan os.Signal, 1)
	interrupt <- os.Interrupt
	return callKeyringWithInterrupt("get", time.Second, func() (string, error) {
		// The pre-loaded interrupt wins the select immediately; this brief
		// sleep just keeps fn from racing it, then the goroutine exits into
		// the buffered result channel (no leak).
		time.Sleep(50 * time.Millisecond)
		return notReturnedSentinel, nil
	}, interrupt)
}

func TestKeyringTimeout_DefaultWhenUnset(t *testing.T) {
	t.Setenv(keyringTimeoutEnvVar, "")

	if got := keyringTimeout(); got != defaultKeyringTimeout {
		t.Fatalf("got %v, want default %v", got, defaultKeyringTimeout)
	}
}

func TestKeyringTimeout_HonoursEnvOverride(t *testing.T) {
	t.Setenv(keyringTimeoutEnvVar, "150ms")

	if got := keyringTimeout(); got != 150*time.Millisecond {
		t.Fatalf("got %v, want 150ms", got)
	}
}

func TestKeyringTimeout_IgnoresInvalidEnvValue(t *testing.T) {
	t.Setenv(keyringTimeoutEnvVar, "not-a-duration")

	if got := keyringTimeout(); got != defaultKeyringTimeout {
		t.Fatalf("got %v, want default %v", got, defaultKeyringTimeout)
	}
}

func TestKeyringTimeout_IgnoresNonPositiveValue(t *testing.T) {
	t.Setenv(keyringTimeoutEnvVar, "0s")

	if got := keyringTimeout(); got != defaultKeyringTimeout {
		t.Fatalf("got %v, want default %v", got, defaultKeyringTimeout)
	}
}

// keyringProviderName must always name something the timeout error can
// point at, including on unrecognised platforms.
func TestKeyringProviderName_NonEmpty(t *testing.T) {
	t.Parallel()

	if keyringProviderName() == "" {
		t.Fatal("keyringProviderName returned empty string")
	}
}
