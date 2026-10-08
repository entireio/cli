package strategy

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// syncBuffer is an io.Writer the test goroutine can poll while another
// goroutine writes to it. bytes.Buffer alone is not safe for that -- the race
// detector flags the concurrent grow/String, and a torn read would make the
// poll below flaky rather than failing honestly.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// TestClearSessionStateWithProgress_AnnouncesLockWait covers the UX half of
// gating the clear: the wait is correct (deleting the state file out from
// under an in-flight write destroys it) but unbounded, and a condensation can
// hold the lock ~30s, so a silent wait reads as a hang. This drives a real
// held lock -- a writer parked inside MutateSessionState -- and asserts the
// notice is printed while the clear is still blocked.
func TestClearSessionStateWithProgress_AnnouncesLockWait(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	const sessionID = "lock-notice-session"
	if err := SaveSessionState(ctx, &SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	writerHolding := make(chan struct{})
	writerMayFinish := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		if err := MutateSessionState(ctx, sessionID, func(*SessionState) error {
			close(writerHolding)
			<-writerMayFinish
			return nil
		}); err != nil {
			t.Errorf("MutateSessionState: %v", err)
		}
	}()
	// Bounded: a setup failure should report the writer's own error rather
	// than hanging to the package timeout with nothing printed.
	select {
	case <-writerHolding:
	case <-time.After(10 * time.Second):
		t.Fatal("writer never acquired the gate; setup failed")
	}

	var errBuf syncBuffer
	cleared := make(chan error, 1)
	go func() {
		cleared <- ClearSessionStateWithProgress(ctx, sessionID, &errBuf, 20*time.Millisecond)
	}()

	// The notice must appear while the clear is still blocked, which is the
	// whole point -- it is useless if it only prints after the wait ends.
	deadline := time.After(5 * time.Second)
	for !strings.Contains(errBuf.String(), "release its state lock") {
		select {
		case <-deadline:
			t.Fatalf("no lock-wait notice while blocked; stderr was %q", errBuf.String())
		case err := <-cleared:
			t.Fatalf("clear returned (%v) before the writer released; stderr %q", err, errBuf.String())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !strings.Contains(errBuf.String(), sessionID) {
		t.Errorf("notice should name the session, got %q", errBuf.String())
	}

	close(writerMayFinish)
	<-writerDone
	if err := <-cleared; err != nil {
		t.Fatalf("clear failed after the lock freed: %v", err)
	}
}

// The uncontended case must stay silent: an unconditional notice would fire on
// every run and train the user to ignore it.
func TestClearSessionStateWithProgress_SilentWhenUncontended(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	const sessionID = "lock-quiet-session"
	if err := SaveSessionState(ctx, &SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	var errBuf syncBuffer
	if err := ClearSessionStateWithProgress(ctx, sessionID, &errBuf, time.Second); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if errBuf.Len() != 0 {
		t.Errorf("expected no output when the lock is free, got %q", errBuf.String())
	}
}

// A reentrant clear must be refused, not silently undone. Holding the gate
// makes the delete safe but not effective: the frame's save writes the state
// straight back, so before this was refused the caller got a nil error and a
// live state file -- doctor would report a session discarded and it would
// reappear.
func TestClearSessionState_RefusesReentrantClear(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	const sessionID = "reentrant-clear"
	if err := SaveSessionState(ctx, &SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	var clearErr error
	if err := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		clearErr = ClearSessionState(ctx, sessionID)
		state.StepCount = 42
		return nil
	}); err != nil {
		t.Fatalf("MutateSessionState: %v", err)
	}

	if clearErr == nil {
		t.Fatal("a reentrant clear must return an error, not a nil that the frame's save then undoes")
	}
	if !strings.Contains(clearErr.Error(), "clearSessionStateLocked") {
		t.Errorf("the error should name the correct alternative, got: %v", clearErr)
	}

	// And the state must still be there, consistent with the refusal.
	after, err := LoadSessionState(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadSessionState: %v", err)
	}
	if after == nil {
		t.Fatal("state should survive a refused clear")
	}
	if after.StepCount != 42 {
		t.Errorf("the frame's own write should have landed; StepCount = %d, want 42", after.StepCount)
	}
}

// TestClearSessionStateWithProgress_RefusesReentrantClear is the regression
// for a deadlock the progress wrapper introduced: it ran the clear on a fresh
// goroutine, and acquireSessionGate keys reentrancy on goroutine ID, so on the
// child the gate looked unheld. The refusal in ClearSessionState never fired
// and the child blocked in flock.AcquireIn on the flock its own parent held,
// while the parent waited for the child -- a permanent hang, printed after the
// lock-wait notice so it named a condensation that did not exist.
//
// This matters more than the bare ClearSessionState case: the doc comment
// points user-facing commands at THIS entry point, so the guard has to hold
// here or it protects only the path nobody is told to use.
func TestClearSessionStateWithProgress_RefusesReentrantClear(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	const sessionID = "reentrant-progress-clear"
	if err := SaveSessionState(ctx, &SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	var errBuf syncBuffer
	returned := make(chan error, 1)
	frameDone := make(chan error, 1)
	go func() {
		frameDone <- MutateSessionState(ctx, sessionID, func(*SessionState) error {
			returned <- ClearSessionStateWithProgress(ctx, sessionID, &errBuf, 20*time.Millisecond)
			return nil
		})
	}()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("a reentrant clear must be refused, not silently undone by the frame's save")
		}
		if !strings.Contains(err.Error(), "clearSessionStateLocked") {
			t.Errorf("the error should name the correct alternative, got: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("deadlock: the clear ran on a goroutine that did not hold the gate, so it blocked on its own parent's flock")
	}

	// The clear's result arrives from INSIDE the mutation, so the frame is
	// still open when the receive above unblocks: it has yet to save and drop
	// the flock. Reporting the frame's own error from its goroutine and
	// returning here raced that save against this test's cleanups -- t.Chdir
	// restores the cwd and t.TempDir deletes the repo, so the save resolves a
	// different git dir (in CI, the real checkout, which the state store's
	// test-isolation guard then refuses) and fails. The t.Errorf that reported
	// it landed on a completed test, which panics the whole package run rather
	// than failing this one test. Join the frame here instead, and assert on
	// its error from the test goroutine.
	select {
	case err := <-frameDone:
		if err != nil {
			t.Fatalf("MutateSessionState: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("MutateSessionState never returned after the refused clear")
	}

	// And it must not have claimed someone else held the lock.
	if strings.Contains(errBuf.String(), "release its state lock") {
		t.Errorf("a reentrant refusal must not print the lock-wait notice; got %q", errBuf.String())
	}
}
