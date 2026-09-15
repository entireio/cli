package binding

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Record-store tests use t.Setenv for config-dir isolation, so none of them
// may call t.Parallel.

// mustLoad reads a record and fails the test on an error, so callers can assert
// on presence alone.
func mustLoad(ctx context.Context, t *testing.T, sessionID string) *SessionRecord {
	t.Helper()
	rec, err := LoadRecord(ctx, sessionID)
	if err != nil {
		t.Fatalf("load %s: %v", sessionID, err)
	}
	return rec
}

// age rewrites a record's UpdatedAt so retention has something to act on.
func age(ctx context.Context, t *testing.T, sessionID string, updatedAt time.Time) {
	t.Helper()
	rec, err := LoadRecord(ctx, sessionID)
	if err != nil || rec == nil {
		t.Fatalf("load %s: rec=%v err=%v", sessionID, rec, err)
	}
	rec.UpdatedAt = updatedAt
	path, err := recordPath(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPruneStaleRecords is the retention the no-repo scan parked.
//
// Every session with a turn-end writes a machine-level record, including one
// that never touches a repo: the cursor advances on a successful scan so repeat
// scans stay cheap, which means a chatty no-repo session leaves a cursor-only
// record behind. Nothing removed them, so ~/.config/entire/sessions grew without
// bound for the life of the machine.
func TestPruneStaleRecords(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	now := time.Now()

	if err := RecordBinding(ctx, "fresh", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	if err := RecordBinding(ctx, "stale", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	age(ctx, t, "stale", now.Add(-RecordRetention-time.Hour))

	pruned, err := PruneStaleRecords(ctx, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}

	if rec := mustLoad(ctx, t, "stale"); rec != nil {
		t.Error("a record past the retention window must be removed")
	}
	if rec := mustLoad(ctx, t, "fresh"); rec == nil {
		t.Error("a record inside the window must survive")
	}
}

// TestPruneStaleRecords_LeavesTheBoundaryAlone keeps the window a window: a
// record exactly at the cutoff is not yet stale.
func TestPruneStaleRecords_LeavesTheBoundaryAlone(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	now := time.Now()

	if err := RecordBinding(ctx, "edge", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	age(ctx, t, "edge", now.Add(-RecordRetention))

	if _, err := PruneStaleRecords(ctx, now); err != nil {
		t.Fatal(err)
	}
	if rec := mustLoad(ctx, t, "edge"); rec == nil {
		t.Error("a record exactly at the cutoff must survive")
	}
}

// TestPruneStaleRecords_EmptyStoreIsNotAnError covers the common case: the
// sweep runs on machines that have never written a record.
func TestPruneStaleRecords_EmptyStoreIsNotAnError(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	pruned, err := PruneStaleRecords(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("an absent store is not an error: %v", err)
	}
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0", pruned)
	}
}

// TestPruneStaleRecords_IgnoresForeignFiles pins that retention only removes
// what it owns: the sessions directory is under the user config dir, and a
// sweep that deleted anything it did not recognize would be a footgun.
func TestPruneStaleRecords_IgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	ctx := context.Background()

	if err := RecordBinding(ctx, "s1", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "sessions", "notes.txt")
	if err := os.WriteFile(foreign, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneStaleRecords(ctx, time.Now().Add(10*365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a file retention does not own must be left alone: %v", err)
	}
}

// TestPruneStaleRecords_RemovesTheLockFileToo pins the other half of the growth
// problem: mutateRecord creates a <record>.lock beside every record, one per
// session, and nothing else reclaims them. Removing only the record would leave
// the directory growing without bound at exactly the same rate.
func TestPruneStaleRecords_RemovesTheLockFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	ctx := context.Background()
	now := time.Now()

	if err := RecordBinding(ctx, "stale", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	age(ctx, t, "stale", now.Add(-RecordRetention-time.Hour))

	lock := filepath.Join(dir, "sessions", "stale.json.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("precondition: the record store keeps a lock file beside each record: %v", err)
	}

	if _, err := PruneStaleRecords(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("a pruned record must take its lock file with it (stat err = %v)", err)
	}
}

// TestRetentionDue covers the scheduling half: the sweep that performs
// retention is only spawned when something nominates it, and zombie sessions —
// the sweep's original nomination — are absent on exactly the machines whose
// record store grows (a session that never touches a repo leaves a record but
// no zombie). Retention therefore has to nominate itself.
func TestRetentionDue(t *testing.T) {
	t.Run("a machine that has never recorded is not due", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		if RetentionDue(context.Background(), time.Now()) {
			t.Error("no store means nothing to prune: spawning a sweep for it is pure cost")
		}
	})

	t.Run("a store that has never been pruned is due", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		ctx := context.Background()
		if err := RecordBinding(ctx, "s1", testMeta(), testEvidence("b", true)); err != nil {
			t.Fatal(err)
		}
		if !RetentionDue(ctx, time.Now()) {
			t.Error("a store with no prune marker must be due")
		}
	})

	t.Run("a prune resets the interval", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		ctx := context.Background()
		now := time.Now()
		if err := RecordBinding(ctx, "s1", testMeta(), testEvidence("b", true)); err != nil {
			t.Fatal(err)
		}
		if _, err := PruneStaleRecords(ctx, now); err != nil {
			t.Fatal(err)
		}
		if RetentionDue(ctx, now.Add(retentionInterval-time.Minute)) {
			t.Error("inside the interval a second sweep buys nothing")
		}
		if !RetentionDue(ctx, now.Add(retentionInterval+time.Minute)) {
			t.Error("past the interval retention must nominate itself again")
		}
	})
}
