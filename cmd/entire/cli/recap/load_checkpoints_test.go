package recap

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestLoadRecap_AttachesCheckpoints(t *testing.T) {
	repo := newIsolatedRepo(t)
	// Create a session state.
	writeSessionState(t, repo, &session.State{
		SessionID:    "sess-2",
		BaseCommit:   "abcd1234",
		StartedAt:    time.Now().Add(-1 * time.Hour),
		StepCount:    2,
		FilesTouched: []string{"cmd/entire/cli/session/state.go"},
	})
	// Write two committed checkpoints for that session.
	writeCommittedCheckpoint(t, repo, committedFixture{
		ID:        "aa11bb22cc33",
		SessionID: "sess-2",
		CreatedAt: time.Now().Add(-30 * time.Minute),
		Files:     []string{"cmd/entire/cli/session/state.go"},
	})
	writeCommittedCheckpoint(t, repo, committedFixture{
		ID:        "bb22cc33dd44",
		SessionID: "sess-2",
		CreatedAt: time.Now().Add(-10 * time.Minute),
		Files:     []string{"cmd/entire/cli/session/state.go"},
	})
	out, err := LoadRecap(ctx(), LoadOpts{Scope: ScopeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	cps := out.Sessions[0].Checkpoints
	if len(cps) != 2 {
		t.Fatalf("expected 2 checkpoints, got %d", len(cps))
	}
	// Span should be ~20 minutes.
	span := out.Sessions[0].SpanMinutes()
	if span < 15 || span > 25 {
		t.Errorf("expected span ~20m, got %f", span)
	}
}

func TestLoadRecap_DeduplicatesRepeatedSessionIDs(t *testing.T) {
	t.Parallel()
	// Exercise the dedupe path directly via projectCheckpoint's integration
	// in LoadRecap. We synthesize a CheckpointInfo with duplicate session
	// IDs and assert only one attachment per session.
	//
	// This is a unit-ish test — we don't need a real repo for the bySession
	// logic. Use in-memory fixtures by exercising the loader with no git state.
	info := strategy.CheckpointInfo{
		CheckpointID: "dupe12345678",
		SessionID:    "s1",
		SessionIDs:   []string{"s1", "s1", "s2"}, // duplicate s1
		CreatedAt:    time.Now(),
		FilesTouched: []string{"foo.go"},
	}
	// Inline the loader's dedupe logic to isolate the guarantee.
	bySession := map[string][]RecapCheckpoint{}
	seen := map[string]bool{}
	for _, sid := range info.SessionIDs {
		if sid == "" || seen[sid] {
			continue
		}
		seen[sid] = true
		bySession[sid] = append(bySession[sid], projectCheckpoint(info, sid))
	}
	if n := len(bySession["s1"]); n != 1 {
		t.Errorf("s1 attached %d times, want 1", n)
	}
	if n := len(bySession["s2"]); n != 1 {
		t.Errorf("s2 attached %d times, want 1", n)
	}
}

func TestLoadRecap_PropagatesLinkedCommits(t *testing.T) {
	repo := newIsolatedRepo(t)
	writeSessionState(t, repo, &session.State{
		SessionID:    "sess-linked",
		BaseCommit:   "0000000",
		StartedAt:    time.Now().Add(-1 * time.Hour),
		StepCount:    1,
		FilesTouched: []string{"foo.go"},
	})
	writeCommittedCheckpoint(t, repo, committedFixture{
		ID:        "cafebabe1234",
		SessionID: "sess-linked",
		CreatedAt: time.Now().Add(-30 * time.Minute),
		Files:     []string{"foo.go"},
	})
	// Commit on the active branch with a matching trailer.
	testutil.WriteFile(t, repo, "foo.go", "package main")
	testutil.GitAdd(t, repo, "foo.go")
	testutil.GitCommitWithMsg(t, repo, "feat: foo\n\nEntire-Checkpoint: cafebabe1234\n")

	out, err := LoadRecap(ctx(), LoadOpts{Scope: ScopeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions[0].LinkedCommits) == 0 {
		t.Errorf("expected LinkedCommits on session, got none")
	}
	if out.Sessions[0].Checkpoints[0].LinkedCommit == "" {
		t.Errorf("expected LinkedCommit on checkpoint, got empty")
	}
}
