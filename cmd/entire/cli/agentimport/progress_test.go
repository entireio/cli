package agentimport

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// progressSessionEvent and progressTurnEvent capture one Progress callback
// invocation each, in call order, for assertion.
type progressSessionEvent struct {
	sessionIndex, sessionTotal int
	agentName, sessionID       string
	turnCount                  int
}

type progressTurnEvent struct {
	sessionIndex, turnIndex, turnCount int
}

// TestRun_ReportsProgress proves SessionStart fires exactly once per session
// with correct totals, and TurnWritten fires exactly turnCount times per
// session, in order, on a fixture with 2 sessions x 2 turns each.
func TestRun_ReportsProgress(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")

	var sessionEvents []progressSessionEvent
	var turnEvents []progressTurnEvent
	progress := &Progress{
		SessionStart: func(sessionIndex, sessionTotal int, agentName, sessionID string, turnCount int) {
			sessionEvents = append(sessionEvents, progressSessionEvent{sessionIndex, sessionTotal, agentName, sessionID, turnCount})
		},
		TurnWritten: func(sessionIndex, turnIndex, turnCount int) {
			turnEvents = append(turnEvents, progressTurnEvent{sessionIndex, turnIndex, turnCount})
		},
	}

	imp := claudeImporter{}
	res, err := Run(context.Background(), repo, imp, Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now:      time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		Progress: progress,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 4 {
		t.Fatalf("want 4 imported, got %+v", res)
	}

	wantAgentName := string(imp.AgentType())
	wantSessions := []progressSessionEvent{
		{sessionIndex: 0, sessionTotal: 2, agentName: wantAgentName, sessionID: "sess1", turnCount: 2},
		{sessionIndex: 1, sessionTotal: 2, agentName: wantAgentName, sessionID: "sess2", turnCount: 2},
	}
	if !reflect.DeepEqual(sessionEvents, wantSessions) {
		t.Fatalf("session events = %+v, want %+v", sessionEvents, wantSessions)
	}

	wantTurns := []progressTurnEvent{
		{sessionIndex: 0, turnIndex: 0, turnCount: 2},
		{sessionIndex: 0, turnIndex: 1, turnCount: 2},
		{sessionIndex: 1, turnIndex: 0, turnCount: 2},
		{sessionIndex: 1, turnIndex: 1, turnCount: 2},
	}
	if !reflect.DeepEqual(turnEvents, wantTurns) {
		t.Fatalf("turn events = %+v, want %+v", turnEvents, wantTurns)
	}
}

// TestRun_NilProgressDoesNotPanic proves a nil Progress (the zero value of
// Options.Progress) behaves identically to a reporter-enabled run: no panic,
// same turn count imported.
func TestRun_NilProgressDoesNotPanic(t *testing.T) {
	t.Parallel()
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)

	repoNil, repoNilDir := initRepoWithCommit(t)
	resNil, err := Run(context.Background(), repoNil, claudeImporter{}, Options{
		RepoRoot: repoNilDir, OverridePath: claudeDir, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	repoWith, repoWithDir := initRepoWithCommit(t)
	resWith, err := Run(context.Background(), repoWith, claudeImporter{}, Options{
		RepoRoot: repoWithDir, OverridePath: claudeDir, Now: now,
		Progress: &Progress{
			SessionStart: func(int, int, string, string, int) {},
			TurnWritten:  func(int, int, int) {},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resNil.TurnsImported != resWith.TurnsImported {
		t.Fatalf("nil progress imported %d turns, reporter-enabled imported %d", resNil.TurnsImported, resWith.TurnsImported)
	}
	if resNil.TurnsImported != 4 {
		t.Fatalf("want 4 imported, got %+v", resNil)
	}
}

// progressRecorder collects Progress callback invocations for assertion.
type progressRecorder struct {
	written []progressTurnEvent
	skipped []progressTurnEvent
}

func (r *progressRecorder) progress() *Progress {
	return &Progress{
		TurnWritten: func(sessionIndex, turnIndex, turnCount int) {
			r.written = append(r.written, progressTurnEvent{sessionIndex, turnIndex, turnCount})
		},
		TurnSkipped: func(sessionIndex, turnIndex, turnCount int) {
			r.skipped = append(r.skipped, progressTurnEvent{sessionIndex, turnIndex, turnCount})
		},
	}
}

// TestRun_ReimportFiresTurnSkippedNotTurnWritten proves a re-import over an
// already-imported corpus (the idempotent-skip path) reports every turn via
// TurnSkipped, in order, and never via TurnWritten — the P2 Codex's pre-push
// review caught: without this, a TTY progress reporter driven only by
// TurnWritten freezes at "turn 0/M" on a fully-skipped session.
func TestRun_ReimportFiresTurnSkippedNotTurnWritten(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")
	opts := Options{RepoRoot: repoDir, OverridePath: claudeDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}

	// First run: no progress, just to populate the store so the second run
	// hits the idempotent-skip path for every turn.
	if _, err := Run(context.Background(), repo, claudeImporter{}, opts); err != nil {
		t.Fatal(err)
	}

	rec := &progressRecorder{}
	opts.Progress = rec.progress()
	res, err := Run(context.Background(), repo, claudeImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 0 || res.TurnsSkipped != 4 {
		t.Fatalf("want 0 imported / 4 skipped on re-import, got %+v", res)
	}

	if len(rec.written) != 0 {
		t.Errorf("TurnWritten fired %d times on a fully-skipped re-import, want 0: %+v", len(rec.written), rec.written)
	}
	wantSkipped := []progressTurnEvent{
		{sessionIndex: 0, turnIndex: 0, turnCount: 2},
		{sessionIndex: 0, turnIndex: 1, turnCount: 2},
		{sessionIndex: 1, turnIndex: 0, turnCount: 2},
		{sessionIndex: 1, turnIndex: 1, turnCount: 2},
	}
	if !reflect.DeepEqual(rec.skipped, wantSkipped) {
		t.Fatalf("TurnSkipped events = %+v, want %+v", rec.skipped, wantSkipped)
	}
}

// TestRun_DryRunFiresTurnSkippedForEveryTurn proves DryRun — which never
// writes — reports every turn via TurnSkipped and never via TurnWritten.
func TestRun_DryRunFiresTurnSkippedForEveryTurn(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")

	rec := &progressRecorder{}
	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDir, DryRun: true,
		Now:      time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		Progress: rec.progress(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 4 {
		// DryRun's Result bookkeeping is unchanged: TurnsImported still means
		// "would import" (see Run's DryRun branch). TurnSkipped is a separate,
		// additive signal that nothing was actually written.
		t.Fatalf("want 4 (would-import), got %+v", res)
	}

	if len(rec.written) != 0 {
		t.Errorf("TurnWritten fired %d times under DryRun, want 0: %+v", len(rec.written), rec.written)
	}
	if len(rec.skipped) != 4 {
		t.Fatalf("TurnSkipped fired %d times under DryRun, want 4: %+v", len(rec.skipped), rec.skipped)
	}
}

// TestRun_MixedSkipAndWriteSatisfiesInvariant proves the documented
// invariant — for every turn, exactly one of TurnWritten/TurnSkipped fires,
// so per-session written+skipped == turnCount — holds when a run mixes
// already-imported sessions with a brand-new one in a single call.
func TestRun_MixedSkipAndWriteSatisfiesInvariant(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")
	opts := Options{RepoRoot: repoDir, OverridePath: claudeDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}

	// Import sess1 and sess2 first, so a second run finds them already
	// imported while a newly-added sess3 is still fresh.
	if _, err := Run(context.Background(), repo, claudeImporter{}, opts); err != nil {
		t.Fatal(err)
	}
	writeFixtureSession(t, claudeDir, "sess3.jsonl")

	rec := &progressRecorder{}
	opts.Progress = rec.progress()
	res, err := Run(context.Background(), repo, claudeImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 || res.TurnsSkipped != 4 {
		t.Fatalf("want 2 imported (sess3) / 4 skipped (sess1+sess2), got %+v", res)
	}

	counts := map[int]struct{ written, skipped int }{}
	for _, ev := range rec.written {
		c := counts[ev.sessionIndex]
		c.written++
		counts[ev.sessionIndex] = c
	}
	for _, ev := range rec.skipped {
		c := counts[ev.sessionIndex]
		c.skipped++
		counts[ev.sessionIndex] = c
	}

	// Discovery is sorted by path, so sess1=0, sess2=1, sess3=2 (each has 2
	// turns per writeFixtureSession).
	wantBySession := map[int]struct{ written, skipped int }{
		0: {written: 0, skipped: 2}, // sess1: already imported
		1: {written: 0, skipped: 2}, // sess2: already imported
		2: {written: 2, skipped: 0}, // sess3: brand new
	}
	if len(counts) != len(wantBySession) {
		t.Fatalf("saw events for %d sessions, want %d: %+v", len(counts), len(wantBySession), counts)
	}
	for sessionIndex, want := range wantBySession {
		got := counts[sessionIndex]
		if got != want {
			t.Errorf("session %d: written=%d skipped=%d, want written=%d skipped=%d",
				sessionIndex, got.written, got.skipped, want.written, want.skipped)
		}
		if got.written+got.skipped != 2 {
			t.Errorf("session %d: written+skipped = %d, want turnCount 2 (invariant violated)",
				sessionIndex, got.written+got.skipped)
		}
	}
}
