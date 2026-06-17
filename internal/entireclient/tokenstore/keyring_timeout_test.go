package tokenstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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
		return "should not be returned", nil
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
