package jsonutil

import (
	"errors"
	"testing"
	"time"
)

var errBusy = errors.New("sharing violation")

func TestRetryRename_RetriesTransientThenSucceeds(t *testing.T) {
	t.Parallel()
	calls := 0
	var slept []time.Duration
	err := retryRename(
		func() error {
			calls++
			if calls < 3 {
				return errBusy
			}
			return nil
		},
		func(err error) bool { return errors.Is(err, errBusy) },
		func(d time.Duration) { slept = append(slept, d) },
	)
	if err != nil {
		t.Fatalf("retryRename: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	want := []time.Duration{renameInitialDelay, 2 * renameInitialDelay}
	if len(slept) != len(want) || slept[0] != want[0] || slept[1] != want[1] {
		t.Fatalf("slept = %v, want exponential backoff %v", slept, want)
	}
}

func TestRetryRename_NonTransientErrorFailsFast(t *testing.T) {
	t.Parallel()
	calls := 0
	permanent := errors.New("no such directory")
	err := retryRename(
		func() error { calls++; return permanent },
		func(err error) bool { return errors.Is(err, errBusy) },
		func(time.Duration) { t.Fatal("must not sleep on a non-transient error") },
	)
	if !errors.Is(err, permanent) || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want the permanent error after exactly one attempt", err, calls)
	}
}

func TestRetryRename_GivesUpAfterBoundedAttempts(t *testing.T) {
	t.Parallel()
	calls, sleeps := 0, 0
	err := retryRename(
		func() error { calls++; return errBusy },
		func(err error) bool { return errors.Is(err, errBusy) },
		func(time.Duration) { sleeps++ },
	)
	if !errors.Is(err, errBusy) {
		t.Fatalf("err = %v, want the last transient error surfaced", err)
	}
	if calls != renameAttempts || sleeps != renameAttempts-1 {
		t.Fatalf("calls = %d, sleeps = %d; want %d attempts and %d waits", calls, sleeps, renameAttempts, renameAttempts-1)
	}
}
