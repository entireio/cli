package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestLoadBackgroundClaimContext_IgnoresLeakedLiveRecordOfEndedSession pins
// that a live background record left on an ended session (SessionEnd completes
// every live record, so it was leaked) does not count as an overlapping task:
// it would otherwise stop every later task in the worktree from claiming its
// new files.
func TestLoadBackgroundClaimContext_IgnoresLeakedLiveRecordOfEndedSession(t *testing.T) {
	// Not parallel: callerSessionRepo uses t.Chdir.
	worktree := callerSessionRepo(t)
	ctx := context.Background()
	launched := time.Now().Add(-time.Minute)
	save := func(id string, phase session.Phase, rec session.TaskRecord) {
		t.Helper()
		state := &SessionState{SessionID: id, BaseCommit: "abc1234", WorktreePath: worktree, StartedAt: launched, Phase: phase, TaskRecords: []session.TaskRecord{rec}}
		if err := SaveSessionState(ctx, state); err != nil {
			t.Fatalf("SaveSessionState(%q) error = %v", id, err)
		}
	}
	save("own", session.PhaseActive, session.TaskRecord{ToolUseID: "toolu_own", AgentID: "a1", StartedAt: launched, Background: true})
	save("ended", session.PhaseEnded, session.TaskRecord{ToolUseID: "call_leaked", AgentID: "ses_leaked", StartedAt: launched.Add(-time.Hour), Background: true})

	claims, ok, err := LoadBackgroundClaimContext(ctx, "own", "toolu_own")
	if err != nil || !ok {
		t.Fatalf("LoadBackgroundClaimContext() = ok %v, err %v", ok, err)
	}
	if claims.Overlapped {
		t.Error("Overlapped = true; a leaked live record on an ended session is not running")
	}

	save("running", session.PhaseActive, session.TaskRecord{ToolUseID: "call_live", AgentID: "ses_live", StartedAt: launched, Background: true})
	claims, _, err = LoadBackgroundClaimContext(ctx, "own", "toolu_own")
	if err != nil {
		t.Fatal(err)
	}
	if !claims.Overlapped {
		t.Error("Overlapped = false; a live record on an active session still overlaps")
	}
}
