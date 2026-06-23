//go:build linux || darwin

package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestFinalizeExitedSessions finalizes an ACTIVE session whose owner process is
// gone, and leaves an ACTIVE session without a recorded owner untouched.
//
// Not parallel: setupAttachTestRepo uses t.Chdir.
func TestFinalizeExitedSessions(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()

	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Owner with a mismatched start fingerprint reads as a reused (dead) PID, a
	// deterministic "agent exited" signal on linux/darwin.
	exited := &session.State{
		SessionID: "exited-session",
		Phase:     session.PhaseActive,
		StartedAt: time.Now(),
		Owner:     &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"},
	}
	// No owner recorded: must be left alone (liveness unknown → timeout fallback).
	noOwner := &session.State{
		SessionID: "no-owner-session",
		Phase:     session.PhaseActive,
		StartedAt: time.Now(),
	}
	for _, s := range []*session.State{exited, noOwner} {
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("save %s: %v", s.SessionID, err)
		}
	}

	states, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if n := finalizeExitedSessions(ctx, states); n != 1 {
		t.Fatalf("finalizeExitedSessions = %d, want 1", n)
	}

	// The exited session is now ended on disk.
	got, err := store.Load(ctx, "exited-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt == nil {
		t.Error("exited session EndedAt = nil, want set")
	}
	if got.Phase != session.PhaseEnded {
		t.Errorf("exited session Phase = %q, want %q", got.Phase, session.PhaseEnded)
	}

	// The owner-less session is untouched.
	got, err = store.Load(ctx, "no-owner-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt != nil {
		t.Error("no-owner session EndedAt set, want nil (left active)")
	}
}

// TestFinalizeExitedSessions_RevalidatesUnderLock guards against the
// time-of-check/time-of-use race: the sweep must re-check OwnerExited on the
// freshly-loaded state, not act on a stale list snapshot. Here the on-disk
// state has a LIVE owner while the snapshot passed to the sweep carries a dead
// one (as if a turn revived the session after the list was taken).
//
// Not parallel: setupAttachTestRepo uses t.Chdir.
func TestFinalizeExitedSessions_RevalidatesUnderLock(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()

	liveOwner, ok := proclive.ResolveOwner()
	if !ok {
		t.Skip("no stable process owner resolvable in this environment")
	}

	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, &session.State{
		SessionID: "revived",
		Phase:     session.PhaseActive,
		StartedAt: time.Now(),
		Owner:     &liveOwner, // on disk: a live owner
	}); err != nil {
		t.Fatal(err)
	}

	// Stale snapshot the sweep sees: same session, but with a dead owner.
	stale := &session.State{
		SessionID: "revived",
		Phase:     session.PhaseActive,
		StartedAt: time.Now(),
		Owner:     &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"},
	}

	if n := finalizeExitedSessions(ctx, []*session.State{stale}); n != 0 {
		t.Fatalf("finalizeExitedSessions = %d, want 0 (revalidation should skip the revived session)", n)
	}

	got, err := store.Load(ctx, "revived")
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt != nil {
		t.Error("revived session was ended despite a live owner on disk")
	}
}
