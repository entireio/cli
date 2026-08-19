package session

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

func TestOwnerLiveness_NilOwnerIsUnknown(t *testing.T) {
	t.Parallel()
	s := &State{Phase: PhaseActive}
	if got := s.OwnerLiveness(); got != proclive.LivenessUnknown {
		t.Errorf("OwnerLiveness(nil owner) = %v, want unknown", got)
	}
}

func TestOwnerExited_NilOwnerIsFalse(t *testing.T) {
	t.Parallel()
	// No owner recorded: behavior must degrade to the timeout heuristic, so
	// OwnerExited reports false regardless of phase.
	s := &State{Phase: PhaseActive}
	if s.OwnerExited() {
		t.Error("OwnerExited(nil owner) = true, want false")
	}
}

func TestOwnerExited_EndedPhaseIsFalse(t *testing.T) {
	t.Parallel()
	// An ENDED session is already finalized: there is nothing left to reclaim,
	// so a dead owner must not re-flag it.
	deadOwner := &proclive.Identity{PID: 999999999, Start: "never"}
	s := &State{Phase: PhaseEnded, Owner: deadOwner}
	if s.OwnerExited() {
		t.Error("OwnerExited(phase=ended) = true, want false")
	}
}

func TestOwnerExited_EndedAtSetIsFalse(t *testing.T) {
	t.Parallel()
	// EndedAt is the other half of "already finalized" — the rest of the CLI
	// filters on it alongside PhaseEnded, so OwnerExited must agree or a
	// finalized session could be swept a second time.
	deadOwner := &proclive.Identity{PID: 999999999, Start: "never"}
	ended := time.Now()
	s := &State{Phase: PhaseIdle, Owner: deadOwner, EndedAt: &ended}
	if s.OwnerExited() {
		t.Error("OwnerExited(EndedAt set) = true, want false")
	}
}
