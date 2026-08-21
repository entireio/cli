//go:build linux || darwin

package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFinalizeExitedSessions finalizes sessions whose owner process is gone
// from every non-ended phase, and leaves a session without a recorded owner
// untouched. The IDLE case is the shape agents without a session-end hook leave
// behind — the agent finished its last turn and then quit (Codex before 0.146,
// or any agent killed before its hook runs) — and must not linger as "active".
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
	deadOwner := func() *proclive.Identity {
		return &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"}
	}
	exitedIDs := []string{"exited-active-session", "exited-idle-session"}
	saved := []*session.State{
		{SessionID: exitedIDs[0], Phase: session.PhaseActive, StartedAt: time.Now(), Owner: deadOwner()},
		{SessionID: exitedIDs[1], Phase: session.PhaseIdle, StartedAt: time.Now(), Owner: deadOwner()},
		// No owner recorded: must be left alone (liveness unknown → timeout fallback).
		{SessionID: "no-owner-session", Phase: session.PhaseActive, StartedAt: time.Now()},
	}
	for _, s := range saved {
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("save %s: %v", s.SessionID, err)
		}
	}

	states, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if n := finalizeExitedSessions(ctx, states); n != len(exitedIDs) {
		t.Fatalf("finalizeExitedSessions = %d, want %d", n, len(exitedIDs))
	}

	// Every exited session is now ended on disk.
	for _, id := range exitedIDs {
		got, err := store.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.EndedAt == nil {
			t.Errorf("%s EndedAt = nil, want set", id)
		}
		if got.Phase != session.PhaseEnded {
			t.Errorf("%s Phase = %q, want %q", id, got.Phase, session.PhaseEnded)
		}
	}

	// The owner-less session is untouched.
	got, err := store.Load(ctx, "no-owner-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt != nil {
		t.Error("no-owner session EndedAt set, want nil (left active)")
	}
}

// TestFinalizeExitedSessions_StampsLastSeenNotNow — the sweep discovers ends
// that already happened, so it must date them when the session was last seen,
// not when the sweep noticed. Stamping "now" dates a week-old abandoned session
// to today, floating it above real recent work in the `entire session resume`
// picker (sessionLastActiveTime prefers EndedAt) and reporting it as just-ended.
//
// Not parallel: setupAttachTestRepo uses t.Chdir.
func TestFinalizeExitedSessions_StampsLastSeenNotNow(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()

	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatal(err)
	}

	lastSeen := time.Now().Add(-72 * time.Hour)
	if err := store.Save(ctx, &session.State{
		SessionID:           "walked-away-session",
		Phase:               session.PhaseIdle,
		StartedAt:           lastSeen.Add(-time.Hour),
		LastInteractionTime: &lastSeen,
		Owner:               &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"},
	}); err != nil {
		t.Fatal(err)
	}

	states, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := finalizeExitedSessions(ctx, states); n != 1 {
		t.Fatalf("finalizeExitedSessions = %d, want 1", n)
	}

	got, err := store.Load(ctx, "walked-away-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt == nil {
		t.Fatal("EndedAt = nil, want the last-seen time")
	}
	if !got.EndedAt.Equal(lastSeen) {
		t.Errorf("EndedAt = %s, want the last-seen time %s (sweep back-stamped 'now')",
			got.EndedAt.Format(time.RFC3339), lastSeen.Format(time.RFC3339))
	}
}

// A hard-killed agent fires neither SubagentStop nor SessionEnd, leaving its
// background record LIVE. The sweep must complete it (no SubagentStop can ever
// arrive) before ending, so the eager condense materializes and removes it.
// Not parallel: setupSubagentEndTestRepo uses t.Chdir.
func TestFinalizeExitedSessions_CompletesLiveTaskRecords(t *testing.T) {
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "sweep-live-record-session"
	mainTranscriptPath, _ := writeSubagentTranscripts(t, "sweeprec1")

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	st := &session.State{
		SessionID: sessionID, BaseCommit: headHash, StartedAt: time.Now(), Phase: session.PhaseActive,
		AgentType: agent.AgentTypeClaudeCode, TranscriptPath: mainTranscriptPath,
		Owner:       &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"},
		TaskRecords: []session.TaskRecord{{ToolUseID: "toolu_sweep1", AgentID: "sweeprec1", StartedAt: time.Now(), SubagentType: "reviewer"}},
	}
	require.NoError(t, store.Save(ctx, st))
	require.Equal(t, 1, finalizeExitedSessions(ctx, []*session.State{st}))

	got, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	assert.True(t, got.FullyCondensed, "sweep condense should have run")
	assert.Empty(t, got.TaskRecords, "completed record must materialize and be removed")
	_, found := readCheckpointTaskFile(ctx, t, repoDir, sessionID, "tasks/toolu_sweep1/agent-sweeprec1.jsonl")
	assert.True(t, found, "swept record's transcript-so-far must materialize under tasks/")
	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	assert.Nil(t, classifySession(got, repo, time.Now()), "doctor must classify the swept session healthy")
}

// TestEndSessionNow_SpentBudgetStillMarksEnded pins the split that makes the
// sweep's condense budget safe: exhausting it may drop the eager condense, but
// never the mark-ended write. Dropping that would leave the session showing as
// active — the exact bug the sweep exists to fix.
//
// Not parallel: setupAttachTestRepo uses t.Chdir.
func TestEndSessionNow_SpentBudgetStillMarksEnded(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()

	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, &session.State{
		SessionID:  "budget-spent-session",
		Phase:      session.PhaseIdle,
		StartedAt:  time.Now(),
		StepCount:  3, // would otherwise be a condense candidate
		BaseCommit: "abcdef1234567890abcdef1234567890abcdef12",
	}); err != nil {
		t.Fatal(err)
	}

	// A deadline already in the past: every condense is skipped from here on.
	spent := time.Now().Add(-time.Second)
	ended, err := endSessionNow(ctx, nil, "budget-spent-session", nil, spent, endedNow)
	if err != nil {
		t.Fatalf("endSessionNow: %v", err)
	}
	if !ended {
		t.Fatal("endSessionNow = false, want true (mark-ended must not be budgeted)")
	}

	got, err := store.Load(ctx, "budget-spent-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt == nil || got.Phase != session.PhaseEnded {
		t.Errorf("session not finalized: phase=%q endedAt=%v", got.Phase, got.EndedAt)
	}
	// The condense was skipped, so PostCommit must still see work pending.
	if got.FullyCondensed {
		t.Error("FullyCondensed = true, want false (condense should have been skipped)")
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
