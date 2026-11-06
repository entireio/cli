package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestOtherSessionActiveSince pins which neighbouring sessions count as having
// had a turn running since a prompt started (#3011): only those homed in the
// same worktree, mid-turn or interacting after the prompt, and not linked to
// the asking session as parent or subagent.
func TestOtherSessionActiveSince(t *testing.T) {
	// Not parallel: callerSessionRepo uses t.Chdir.
	worktree := callerSessionRepo(t)
	ctx := context.Background()
	since := time.Now().Add(-10 * time.Minute)
	before := since.Add(-time.Minute)
	after := since.Add(time.Minute)
	stuck := time.Now().Add(-2 * session.StuckActiveThreshold)

	save := func(state *SessionState) {
		t.Helper()
		if state.WorktreePath == "" {
			state.WorktreePath = worktree
		}
		state.BaseCommit = "abc1234"
		state.StartedAt = stuck
		if err := SaveSessionState(ctx, state); err != nil {
			t.Fatalf("SaveSessionState(%q) error = %v", state.SessionID, err)
		}
	}
	save(&SessionState{SessionID: "own", Phase: session.PhaseActive, LastInteractionTime: &after,
		TaskRecords: []session.TaskRecord{{ToolUseID: "call_worker", AgentID: "worker"}}})

	tests := []struct {
		name    string
		other   *SessionState
		related []string
		want    bool
	}{
		{"idle before the prompt", &SessionState{SessionID: "other", Phase: session.PhaseIdle, LastInteractionTime: &before}, nil, false},
		{"turn ended during the prompt", &SessionState{SessionID: "other", Phase: session.PhaseIdle, LastInteractionTime: &after}, nil, true},
		{"mid-turn since before the prompt", &SessionState{SessionID: "other", Phase: session.PhaseActive, LastInteractionTime: &before}, nil, true},
		{"stuck active", &SessionState{SessionID: "other", Phase: session.PhaseActive, LastInteractionTime: &stuck}, nil, false},
		{"ended before the prompt", &SessionState{SessionID: "other", Phase: session.PhaseEnded, LastInteractionTime: &before, EndedAt: &before}, nil, false},
		{"other worktree", &SessionState{SessionID: "other", Phase: session.PhaseActive, LastInteractionTime: &after, WorktreePath: t.TempDir()}, nil, false},
		{"related", &SessionState{SessionID: "other", Phase: session.PhaseActive, LastInteractionTime: &after}, []string{"other"}, false},
		{"own subagent session", &SessionState{SessionID: "worker", Phase: session.PhaseActive, LastInteractionTime: &after}, nil, false},
		{"own parent session", &SessionState{SessionID: "other", Phase: session.PhaseActive, LastInteractionTime: &after,
			TaskRecords: []session.TaskRecord{{ToolUseID: "call_own", AgentID: "own"}}}, nil, false},
	}
	// Cases share the repository's session store, so they run in sequence
	// rather than as parallel subtests, each removing its session afterwards.
	for _, tt := range tests {
		save(tt.other)
		got, err := OtherSessionActiveSince(ctx, worktree, "own", tt.related, since)
		if err != nil {
			t.Fatalf("%s: OtherSessionActiveSince() error = %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: OtherSessionActiveSince() = %v, want %v", tt.name, got, tt.want)
		}
		if err := ClearSessionState(ctx, tt.other.SessionID); err != nil {
			t.Fatalf("ClearSessionState(%q) error = %v", tt.other.SessionID, err)
		}
	}
}
