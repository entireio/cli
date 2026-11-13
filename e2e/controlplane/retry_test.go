package controlplane

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPolicy(t *testing.T) retryPolicy {
	t.Helper()
	return retryPolicy{attempts: 3, attemptTimeout: time.Minute, delay: time.Millisecond, runDir: t.TempDir(), log: io.Discard}
}

func TestRetryLogin_SucceedsAfterFailures(t *testing.T) {
	t.Parallel()
	p := testPolicy(t)
	calls, retries := 0, 0
	err := retryLogin(context.Background(), p, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "page: github.com/login", errors.New("login stalled")
		}
		return "", nil
	}, func(context.Context) { retries++ })
	if err != nil {
		t.Fatalf("retryLogin: %v", err)
	}
	if calls != 3 || retries != 2 {
		t.Fatalf("calls = %d, retries = %d; want 3 and 2", calls, retries)
	}
	for _, n := range []string{"1", "2"} {
		data, err := os.ReadFile(filepath.Join(p.runDir, "login-attempt-"+n+".txt"))
		if err != nil {
			t.Fatalf("attempt %s diagnosis: %v", n, err)
		}
		if got := string(data); !strings.Contains(got, "attempt "+n+"/3: login stalled") || !strings.Contains(got, "page: github.com/login") {
			t.Fatalf("attempt %s diagnosis = %q", n, got)
		}
	}
	if _, err := os.Stat(filepath.Join(p.runDir, "login-attempt-3.txt")); !os.IsNotExist(err) {
		t.Fatalf("successful attempt left a diagnosis: %v", err)
	}
}

func TestRetryLogin_GivesUpAfterAllAttempts(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retryLogin(context.Background(), testPolicy(t), func(context.Context) (string, error) {
		calls++
		return "", errors.New("login stalled")
	}, func(context.Context) {})
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "attempt 3/3: login stalled") {
		t.Fatalf("err = %v, want every attempt reported", err)
	}
}

func TestRetryLogin_StopsOnPermanentFailure(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retryLogin(context.Background(), testPolicy(t), func(context.Context) (string, error) {
		calls++
		return "", permanent(errors.New("GitHub rejected the username or password"))
	}, func(context.Context) { t.Error("retried after a permanent failure") })
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if !errors.Is(err, errLoginPermanent) {
		t.Fatalf("err = %v, want errLoginPermanent", err)
	}
	if got, want := err.Error(), "attempt 1/3: GitHub rejected the username or password"; got != want {
		t.Fatalf("err = %q, want %q", got, want)
	}
}

func TestRetryLogin_HonorsCancellationBetweenAttempts(t *testing.T) {
	t.Parallel()
	p := testPolicy(t)
	p.delay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryLogin(ctx, p, func(context.Context) (string, error) {
		calls++
		cancel()
		return "", errors.New("login stalled")
	}, func(context.Context) { t.Error("retried after cancellation") })
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTruncateText(t *testing.T) {
	t.Parallel()
	if got := truncateText("short", 10); got != "short" {
		t.Fatalf("truncateText under the limit = %q", got)
	}
	// "é" is two bytes; a cut at byte 2 would split it.
	if got, want := truncateText("aé bc", 2), "a\n[truncated]"; got != want {
		t.Fatalf("truncateText = %q, want %q", got, want)
	}
}
