package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCallKeyringWithContext_ReturnsValueWhenFast(t *testing.T) {
	t.Parallel()

	got, err := callKeyringWithContext(context.Background(), "get", func() (string, error) {
		return "token", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "token" {
		t.Fatalf("got = %q, want %q", got, "token")
	}
}

func TestCallKeyringWithContext_PropagatesInnerError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("backend exploded")
	_, err := callKeyringWithContext(context.Background(), "get", func() (string, error) {
		return "", sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want %v wrapped", err, sentinel)
	}
}

func TestCallKeyringWithContext_DeadlineExceeded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := callKeyringWithContext(ctx, "get", func() (string, error) {
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

func TestCallKeyringWithContext_ExternalCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := callKeyringWithContext(ctx, "get", func() (string, error) {
		time.Sleep(5 * time.Second)
		return "should not be returned", nil
	})
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled wrapped, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("plain cancel should not surface as DeadlineExceeded: %v", err)
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

func TestNewKeyringContext_HasDeadline(t *testing.T) {
	t.Setenv(keyringTimeoutEnvVar, "250ms")

	ctx, cancel := newKeyringContext()
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected context with deadline, got none")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 250*time.Millisecond {
		t.Fatalf("deadline out of expected window: remaining=%s", remaining)
	}
}
