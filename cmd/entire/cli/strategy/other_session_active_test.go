package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
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

	// A Droid Worker that "own" launched, whose transcript names it before
	// any task record on "own" does.
	ownWorkerTranscript := filepath.Join(t.TempDir(), "own-worker.jsonl")
	writeDroidSessionStart(t, ownWorkerTranscript, "own-worker", "own")
	otherWorkerTranscript := filepath.Join(t.TempDir(), "other-worker.jsonl")
	writeDroidSessionStart(t, otherWorkerTranscript, "other-worker", "other")

	type overlapCase struct {
		name    string
		other   *SessionState
		related []string
		want    bool
	}
	tests := []overlapCase{
		{"Worker own launched, before its first turn ends", &SessionState{SessionID: "own-worker", Phase: session.PhaseActive,
			LastInteractionTime: &after, AgentType: agent.AgentTypeFactoryAIDroid, TranscriptPath: ownWorkerTranscript}, nil, false},
		{"Worker another session launched", &SessionState{SessionID: "other-worker", Phase: session.PhaseActive,
			LastInteractionTime: &after, AgentType: agent.AgentTypeFactoryAIDroid, TranscriptPath: otherWorkerTranscript}, nil, true},
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
	if live, ok := proclive.IdentityOf(os.Getpid()); ok {
		gone := live
		gone.Start = "not-" + live.Start // PID reuse reads as an exited owner
		tests = append(tests,
			overlapCase{"long turn, owner alive", &SessionState{SessionID: "other", Phase: session.PhaseActive, LastInteractionTime: &stuck, Owner: &live}, nil, true},
			overlapCase{"mid-turn, owner exited", &SessionState{SessionID: "other", Phase: session.PhaseActive, LastInteractionTime: &before, Owner: &gone}, nil, false},
		)
	}
	// Cases share the repository's session store, so they run in sequence
	// rather than as parallel subtests, each removing its session afterwards.
	for _, tt := range tests {
		save(tt.other)
		got, err := OtherSessionActiveSince(ctx, worktree, "own", tt.related, since, false)
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

	// For one of own's tasks, own's Workers run its other tasks, so they count.
	for _, worker := range []*SessionState{
		{SessionID: "worker", Phase: session.PhaseActive, LastInteractionTime: &after},
		{SessionID: "own-worker", Phase: session.PhaseActive, LastInteractionTime: &after,
			AgentType: agent.AgentTypeFactoryAIDroid, TranscriptPath: ownWorkerTranscript},
	} {
		save(worker)
		got, err := OtherSessionActiveSince(ctx, worktree, "own", nil, since, true)
		if err != nil {
			t.Fatalf("%s for a task: OtherSessionActiveSince() error = %v", worker.SessionID, err)
		}
		if !got {
			t.Errorf("%s for a task: OtherSessionActiveSince() = false, want true", worker.SessionID)
		}
		if err := ClearSessionState(ctx, worker.SessionID); err != nil {
			t.Fatalf("ClearSessionState(%q) error = %v", worker.SessionID, err)
		}
	}
}

// writeDroidSessionStart writes the session_start line a Factory AI Droid
// Worker's transcript opens with, naming the session that launched it.
func writeDroidSessionStart(t *testing.T, path, sessionID, parentSessionID string) {
	t.Helper()
	line := `{"type":"session_start","id":"` + sessionID + `","title":"# Task Tool Invocation Subagent type: worker",` +
		`"callingSessionId":"` + parentSessionID + `","callingToolUseId":"toolu_worker","version":2}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
