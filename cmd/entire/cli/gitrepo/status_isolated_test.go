package gitrepo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
)

// The isolated variant is for walks over a FOREIGN worktree (cross-repo
// adoption): its breach or cancellation must not arm the process-wide latch
// that would degrade the launching repo's own capture, while an already-armed
// latch is still honored.
func TestStatusWithBudgetLatch_IsolatedNeverArmsLatch(t *testing.T) {
	SetStatusBudgetBreachedForTesting(false)
	t.Cleanup(func() { SetStatusBudgetBreachedForTesting(false) })
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	walk := func() (git.Status, error) { <-blocked; return git.Status{}, nil }

	_, err := statusWithBudgetLatch(context.Background(), t.TempDir(), 10*time.Millisecond, false, walk)
	if !errors.Is(err, ErrStatusBudgetExceeded) {
		t.Fatalf("err = %v, want the budget sentinel", err)
	}
	if statusBudgetBreached.Load() {
		t.Fatal("an isolated breach must not arm the process-wide latch")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = statusWithBudgetLatch(ctx, t.TempDir(), time.Second, false, walk)
	if !errors.Is(err, ErrStatusBudgetExceeded) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancellation carrying the sentinel", err)
	}
	if statusBudgetBreached.Load() {
		t.Fatal("an isolated cancellation must not arm the process-wide latch")
	}

	// The default variant does arm it, and the isolated one then honors it.
	if _, err = statusWithBudgetLatch(context.Background(), t.TempDir(), 10*time.Millisecond, true, walk); !errors.Is(err, ErrStatusBudgetExceeded) {
		t.Fatalf("err = %v, want the budget sentinel from the default variant", err)
	}
	if !statusBudgetBreached.Load() {
		t.Fatal("the default variant must arm the latch on breach")
	}
	_, err = statusWithBudgetLatch(context.Background(), t.TempDir(), time.Second, false, func() (git.Status, error) {
		t.Fatal("walk must not run once the latch is armed")
		return nil, errors.New("unreachable")
	})
	if !errors.Is(err, ErrStatusBudgetExceeded) {
		t.Fatalf("err = %v, want fast failure under an armed latch", err)
	}
}
