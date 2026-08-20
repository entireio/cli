//go:build linux || darwin

package session

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

// nonEndedPhases are the phases from which a session is still reclaimable.
// ACTIVE means the agent died mid-turn; IDLE means it finished its turn and
// then quit — the shape agents without a session-end hook leave behind, and the
// one that lingered as "active" in `entire status` until StaleSessionThreshold
// hard-deleted it.
var nonEndedPhases = []Phase{PhaseActive, PhaseIdle}

func TestOwnerExited_DeadOwnerNonEndedPhasesIsTrue(t *testing.T) {
	t.Parallel()
	// Our own PID is alive, but a mismatched start fingerprint makes proclive
	// treat it as a reused (dead) PID — a deterministic "owner gone" signal.
	exitedOwner := &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"}
	for _, phase := range nonEndedPhases {
		s := &State{Phase: phase, Owner: exitedOwner}
		if got := s.OwnerLiveness(); got != proclive.LivenessDead {
			t.Fatalf("OwnerLiveness(phase=%s) = %v, want dead", phase, got)
		}
		if !s.OwnerExited() {
			t.Errorf("OwnerExited(phase=%s, dead owner) = false, want true", phase)
		}
	}
}

func TestOwnerExited_LiveOwnerNonEndedPhasesIsFalse(t *testing.T) {
	t.Parallel()
	// A faithfully-captured identity of a live process must NOT read as exited —
	// for IDLE this is the common case of an agent sitting at its prompt between
	// turns, which must never be swept.
	id, ok := proclive.ResolveOwner()
	if !ok {
		t.Skip("no stable owner resolved in this environment")
	}
	for _, phase := range nonEndedPhases {
		s := &State{Phase: phase, Owner: &id}
		if s.OwnerExited() {
			t.Errorf("OwnerExited(phase=%s, live owner) = true, want false", phase)
		}
	}
}
