package recap

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

func TestNextAction_CommitWhenUncommittedCheckpointsExist(t *testing.T) {
	t.Parallel()
	s := RecapSession{
		IsActive:    true,
		Checkpoints: []RecapCheckpoint{{LinkedCommit: ""}},
	}
	if got := NextAction(s); got != ActionCommit {
		t.Errorf("NextAction = %q, want %q", got, ActionCommit)
	}
}

func TestNextAction_CleanWhenEndedOver7DaysAgo(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-10 * 24 * time.Hour)
	s := RecapSession{
		EndedAt:     &past,
		Checkpoints: []RecapCheckpoint{{LinkedCommit: "abc"}}, // committed
	}
	if got := NextAction(s); got != ActionClean {
		t.Errorf("NextAction = %q, want %q", got, ActionClean)
	}
}

func TestNextAction_ResumeWhenActiveAndCommitted(t *testing.T) {
	t.Parallel()
	s := RecapSession{
		IsActive:    true,
		Checkpoints: []RecapCheckpoint{{LinkedCommit: "abc"}},
	}
	if got := NextAction(s); got != ActionResume {
		t.Errorf("NextAction = %q, want %q", got, ActionResume)
	}
}

func TestNextAction_ResumeWhenIdle(t *testing.T) {
	t.Parallel()
	s := RecapSession{
		Phase:       session.Phase("IDLE"),
		Checkpoints: []RecapCheckpoint{{LinkedCommit: "abc"}},
	}
	if got := NextAction(s); got != ActionResume {
		t.Errorf("NextAction = %q, want %q", got, ActionResume)
	}
}

func TestNextAction_NoneWhenSettled(t *testing.T) {
	t.Parallel()
	recent := time.Now().Add(-1 * time.Hour)
	s := RecapSession{
		EndedAt:     &recent,
		IsActive:    false,
		Checkpoints: []RecapCheckpoint{{LinkedCommit: "abc"}},
	}
	if got := NextAction(s); got != ActionNone {
		t.Errorf("NextAction = %q, want %q (none)", got, ActionNone)
	}
}

func TestNextAction_CommitBeatsClean(t *testing.T) {
	t.Parallel()
	// Ended long ago AND has uncommitted work — commit wins (priority 1).
	past := time.Now().Add(-30 * 24 * time.Hour)
	s := RecapSession{
		EndedAt:     &past,
		Checkpoints: []RecapCheckpoint{{LinkedCommit: ""}},
	}
	if got := NextAction(s); got != ActionCommit {
		t.Errorf("NextAction = %q, want %q (commit beats clean)", got, ActionCommit)
	}
}
