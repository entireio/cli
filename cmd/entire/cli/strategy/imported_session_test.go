package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// saveImportedState sets up an isolated repo (commit + chdir) and writes a
// read-only imported session state, returning its id. Not parallel (t.Chdir).
func saveImportedState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	old := time.Now().Add(-24 * time.Hour) // past both stale and grace thresholds
	const sid = "imported-session"
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	if err := store.Save(context.Background(), &session.State{
		SessionID: sid, Kind: session.KindImported,
		Phase: session.PhaseEnded, StartedAt: old, EndedAt: &old,
	}); err != nil {
		t.Fatalf("save imported state: %v", err)
	}
	return sid
}

// Imported sessions are read-only and commit-less (no shadow branch, empty
// BaseCommit); neither cleanup path may purge or flag them.
func TestImportedSessions_SurviveCleanup(t *testing.T) {
	sid := saveImportedState(t)
	ctx := context.Background()

	states, err := NewManualCommitStrategy().listAllSessionStates(ctx)
	if err != nil {
		t.Fatalf("listAllSessionStates: %v", err)
	}
	if !containsSessionID(states, sid) {
		t.Error("listAllSessionStates purged the imported session")
	}

	items, err := ListOrphanedSessionStates(ctx)
	if err != nil {
		t.Fatalf("ListOrphanedSessionStates: %v", err)
	}
	for _, it := range items {
		if it.ID == sid {
			t.Error("entire clean flagged the imported session as orphaned")
		}
	}
}

func containsSessionID(states []*SessionState, sid string) bool {
	for _, s := range states {
		if s.SessionID == sid {
			return true
		}
	}
	return false
}
