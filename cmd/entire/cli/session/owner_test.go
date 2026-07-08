package session

import (
	"testing"

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

func TestOwnerExited_NonActivePhaseIsFalse(t *testing.T) {
	t.Parallel()
	// Even with a dead owner, a non-ACTIVE session is not "exited" — there's no
	// live turn to have been orphaned.
	deadOwner := &proclive.Identity{PID: 999999999, Start: "never"}
	for _, phase := range []Phase{PhaseIdle, PhaseEnded} {
		s := &State{Phase: phase, Owner: deadOwner}
		if s.OwnerExited() {
			t.Errorf("OwnerExited(phase=%s) = true, want false", phase)
		}
	}
}
