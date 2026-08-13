package binding

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Record-store tests use t.Setenv for config-dir isolation, so none of them
// may call t.Parallel.

func testMeta() SessionMeta {
	return SessionMeta{
		AgentType:      "claude-code",
		TranscriptPath: "/tmp/transcript.jsonl",
		LaunchRoot:     "/tmp/launch",
	}
}

func testRepoIdentity(n string) RepoIdentity {
	return RepoIdentity{
		WorktreeRoot: "/repos/" + n,
		CommonDir:    "/repos/" + n + "/.git",
	}
}

func testEvidence(n string, enabled bool) Evidence {
	return Evidence{Repo: testRepoIdentity(n), Enabled: enabled}
}

func TestRecordBinding_CreatesRecordOnFirstWrite(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected record")
	}
	if rec.Version != 2 {
		t.Errorf("version = %d, want 2", rec.Version)
	}
	if rec.SessionID != "sess-1" {
		t.Errorf("session id = %q", rec.SessionID)
	}
	if rec.AgentType != "claude-code" || rec.TranscriptPath != "/tmp/transcript.jsonl" || rec.LaunchRoot != "/tmp/launch" {
		t.Errorf("meta not stored: %+v", rec)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		t.Error("timestamps must be set")
	}
	if len(rec.BoundRepos) != 1 {
		t.Fatalf("bound repos = %d, want 1", len(rec.BoundRepos))
	}
	br := rec.BoundRepos[0]
	if br.CommonDir != "/repos/b/.git" || br.WorktreeRoot != "/repos/b" {
		t.Errorf("bound repo identity = %+v", br.RepoIdentity)
	}
	if br.EvidenceCount != 1 {
		t.Errorf("evidence count = %d, want 1", br.EvidenceCount)
	}
	if !br.Enabled {
		t.Error("enabled must be true")
	}
	if br.FirstEvidenceAt.IsZero() || br.LastEvidenceAt.IsZero() {
		t.Error("evidence timestamps must be set")
	}
	// Equality, not ordering: the single-clock contract means every timestamp
	// in one mutation is the SAME reading. Note the real enforcement is the
	// mutateRecord callback signature (callers receive the reading instead of
	// calling time.Now); this assertion documents the contract but two fast
	// consecutive Now() calls can land on one tick, so it may not bite alone.
	if !rec.UpdatedAt.Equal(br.LastEvidenceAt) || !rec.UpdatedAt.Equal(br.FirstEvidenceAt) {
		t.Errorf("first-write timestamps must be one clock reading: UpdatedAt %v, LastEvidenceAt %v, FirstEvidenceAt %v",
			rec.UpdatedAt, br.LastEvidenceAt, br.FirstEvidenceAt)
	}

	if runtime.GOOS != "windows" {
		recPath := filepath.Join(userdirs.Config(), "sessions", "sess-1.json")
		info, err := os.Stat(recPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("record perm = %o, want 600", perm)
		}
		dirInfo, err := os.Stat(filepath.Dir(recPath))
		if err != nil {
			t.Fatal(err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("sessions dir perm = %o, want 700", perm)
		}
	}
}

func TestRecordBinding_SameRepoBumpsCounters(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	first, err := LoadRecord(ctx, "sess-1")
	if err != nil || first == nil {
		t.Fatalf("load after first write: rec=%v err=%v", first, err)
	}
	firstAt := first.BoundRepos[0].FirstEvidenceAt

	time.Sleep(5 * time.Millisecond) // make LastEvidenceAt advancement observable
	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.BoundRepos) != 1 {
		t.Fatalf("same repo must not duplicate, got %d entries", len(rec.BoundRepos))
	}
	br := rec.BoundRepos[0]
	if br.EvidenceCount != 2 {
		t.Errorf("evidence count = %d, want 2", br.EvidenceCount)
	}
	if !br.FirstEvidenceAt.Equal(firstAt) {
		t.Errorf("first evidence at changed: %v → %v", firstAt, br.FirstEvidenceAt)
	}
	if !br.LastEvidenceAt.After(firstAt) {
		t.Errorf("last evidence at did not advance: first=%v last=%v", firstAt, br.LastEvidenceAt)
	}
}

func TestRecordBinding_SecondRepoAppended(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("c", false)); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.BoundRepos) != 2 {
		t.Fatalf("bound repos = %d, want 2", len(rec.BoundRepos))
	}
	if rec.BoundRepos[1].CommonDir != "/repos/c/.git" {
		t.Errorf("second repo = %+v", rec.BoundRepos[1])
	}
	if rec.BoundRepos[1].Enabled {
		t.Error("second repo must record enabled=false")
	}
}

func TestRecordBinding_SameCloneDifferentWorktreeRefreshesIdentity(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	// Entries are keyed by CommonDir (clone identity); evidence from a
	// different worktree of the same clone must refresh the whole embedded
	// identity so the stored (WorktreeRoot, Enabled) pair stays coherent.
	commonDir := "/repos/b/.git"
	first := Evidence{Repo: RepoIdentity{WorktreeRoot: "/repos/b", CommonDir: commonDir}, Enabled: true}
	second := Evidence{Repo: RepoIdentity{WorktreeRoot: "/repos/b-wt2", CommonDir: commonDir}, Enabled: false}

	if err := RecordBinding(ctx, "sess-1", testMeta(), first); err != nil {
		t.Fatal(err)
	}
	if err := RecordBinding(ctx, "sess-1", testMeta(), second); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("same clone must not duplicate, got %+v", rec)
	}
	br := rec.BoundRepos[0]
	if br.WorktreeRoot != "/repos/b-wt2" {
		t.Errorf("worktree root = %q, want latest %q (stale root makes (WorktreeRoot, Enabled) incoherent)",
			br.WorktreeRoot, "/repos/b-wt2")
	}
	if br.Enabled {
		t.Error("enabled must reflect the latest observation (false)")
	}
	if br.EvidenceCount != 2 {
		t.Errorf("evidence count = %d, want 2", br.EvidenceCount)
	}
}

func TestMutateRecord_LockContentionErrorsInsteadOfHanging(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	path, err := recordPath("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// Pin the ceiling itself: the 2s value is part of the never-block
	// contract this test exists for; a silently raised ceiling would pass a
	// loose elapsed check.
	if recordLockTimeout != 2*time.Second {
		t.Fatalf("recordLockTimeout = %v, the never-block contract assumes 2s — update this test deliberately if the ceiling changes", recordLockTimeout)
	}

	// Hook contexts carry no deadline; the record write must still give up
	// instead of blocking the hook behind another lock holder. This runs the
	// real ceiling once (~2s) because the deadline-less ctx IS the contract.
	start := time.Now()
	err = RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("RecordBinding must error while the lock is held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error must carry context.DeadlineExceeded, got %v", err)
	}
	if elapsed > recordLockTimeout+2*time.Second {
		t.Errorf("lock wait took %v, must be bounded near the %v ceiling", elapsed, recordLockTimeout)
	}
}

func TestRecordBinding_LaterEmptyMetaDoesNotClobber(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	if err := RecordBinding(ctx, "sess-1", SessionMeta{}, testEvidence("c", false)); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.AgentType != "claude-code" || rec.TranscriptPath != "/tmp/transcript.jsonl" || rec.LaunchRoot != "/tmp/launch" {
		t.Errorf("empty meta clobbered stored meta: %+v", rec)
	}
}

func TestLoadRecord_AbsentReturnsNilNil(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	rec, err := LoadRecord(context.Background(), "never-written")
	if err != nil {
		t.Fatalf("absent record must not error: %v", err)
	}
	if rec != nil {
		t.Fatalf("absent record must be nil, got %+v", rec)
	}
}

func TestLoadRecord_MalformedFileErrors(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	dir := filepath.Join(userdirs.Config(), "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess-1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRecord(context.Background(), "sess-1"); err == nil {
		t.Fatal("malformed record must error")
	}
}

func TestRecordBinding_InvalidSessionIDRejected(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordBinding(ctx, "../evil", testMeta(), testEvidence("b", true)); err == nil {
		t.Fatal("invalid session ID must error")
	}
	if _, err := os.Stat(filepath.Join(userdirs.Config(), "sessions")); !os.IsNotExist(err) {
		t.Errorf("nothing must be created for invalid session ID: %v", err)
	}
	if _, err := LoadRecord(ctx, "../evil"); err == nil {
		t.Error("LoadRecord must reject invalid session ID")
	}
}

func TestRecordBinding_RefusesNewerRecordVersion(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	dir := filepath.Join(userdirs.Config(), "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sess-1.json")
	newer := []byte(`{"version":3,"session_id":"sess-1","future_field":"must survive"}`)
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true)); err == nil {
		t.Fatal("newer-version record must refuse rewrite (unknown fields would be dropped)")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newer) {
		t.Errorf("newer-version record was modified:\n%s", got)
	}

	// Current-version round-trip is unaffected by the gate.
	if err := RecordBinding(ctx, "sess-2", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadRecord(ctx, "sess-2")
	if err != nil || rec == nil || rec.Version != CurrentRecordVersion {
		t.Fatalf("current-version round-trip broken: rec=%+v err=%v", rec, err)
	}
	if err := RecordBinding(ctx, "sess-2", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatalf("current-version record must accept rewrites: %v", err)
	}
}

func TestRecordEvidenceAndAdvanceCursor_WritesEvidenceAndCursorTogether(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	evs := []Evidence{testEvidence("b", true), testEvidence("c", false)}
	if err := RecordEvidenceAndAdvanceCursor(ctx, "sess-1", testMeta(), evs, 5, false); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil || rec == nil {
		t.Fatalf("load: rec=%v err=%v", rec, err)
	}
	if len(rec.BoundRepos) != 2 {
		t.Fatalf("bound repos = %d, want 2 (evidence must land in the same mutation as the cursor)", len(rec.BoundRepos))
	}
	if rec.LastScannedTranscriptCursor != 5 {
		t.Errorf("cursor = %d, want 5", rec.LastScannedTranscriptCursor)
	}
	if rec.BoundRepos[0].CommonDir != testRepoIdentity("b").CommonDir || rec.BoundRepos[1].CommonDir != testRepoIdentity("c").CommonDir {
		t.Errorf("bound repos out of first-seen order: %+v", rec.BoundRepos)
	}
	if !rec.BoundRepos[0].Enabled || rec.BoundRepos[1].Enabled {
		t.Errorf("enabled flags not preserved per evidence: %+v", rec.BoundRepos)
	}
}

func TestRecordEvidenceAndAdvanceCursor_NoEvidenceCreatesCursorOnlyRecord(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordEvidenceAndAdvanceCursor(ctx, "sess-1", testMeta(), nil, 42, false); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected record to be created ('scanned, nothing found yet')")
	}
	if rec.Version != 2 {
		t.Errorf("version = %d, want 2", rec.Version)
	}
	if rec.LastScannedTranscriptCursor != 42 {
		t.Errorf("cursor = %d, want 42", rec.LastScannedTranscriptCursor)
	}
	if want := testMeta(); rec.AgentType != want.AgentType || rec.TranscriptPath != want.TranscriptPath || rec.LaunchRoot != want.LaunchRoot {
		t.Errorf("meta not stored: %+v", rec)
	}
	if len(rec.BoundRepos) != 0 {
		t.Errorf("cursor-only record must have no bound repos: %+v", rec.BoundRepos)
	}
}

func TestRecordEvidenceAndAdvanceCursor_MonotonicWithoutReset(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordEvidenceAndAdvanceCursor(ctx, "sess-1", testMeta(), nil, 50, false); err != nil {
		t.Fatal(err)
	}
	// A racing hook reporting an older position must never regress the cursor.
	if err := RecordEvidenceAndAdvanceCursor(ctx, "sess-1", testMeta(), nil, 30, false); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil || rec == nil {
		t.Fatalf("load: rec=%v err=%v", rec, err)
	}
	if rec.LastScannedTranscriptCursor != 50 {
		t.Errorf("cursor = %d, want 50 (must not regress without reset)", rec.LastScannedTranscriptCursor)
	}
}

func TestRecordEvidenceAndAdvanceCursor_ResetPermitsRegression(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordEvidenceAndAdvanceCursor(ctx, "sess-1", testMeta(), nil, 50, false); err != nil {
		t.Fatal(err)
	}
	// Truncated/rotated transcript: the caller passes reset=true and the
	// cursor must follow it down, or later turns full-rescan forever.
	if err := RecordEvidenceAndAdvanceCursor(ctx, "sess-1", testMeta(), nil, 3, true); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil || rec == nil {
		t.Fatalf("load: rec=%v err=%v", rec, err)
	}
	if rec.LastScannedTranscriptCursor != 3 {
		t.Errorf("cursor = %d, want 3 (reset must permit regression)", rec.LastScannedTranscriptCursor)
	}
}

func TestRecordEvidenceAndAdvanceCursor_DoesNotClobberMeta(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	if err := RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true)); err != nil {
		t.Fatal(err)
	}
	if err := RecordEvidenceAndAdvanceCursor(ctx, "sess-1", SessionMeta{AgentType: "other"}, nil, 7, false); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil || rec == nil {
		t.Fatalf("load: rec=%v err=%v", rec, err)
	}
	if want := testMeta().AgentType; rec.AgentType != want {
		t.Errorf("meta was clobbered: agent type = %q, want %q", rec.AgentType, want)
	}
	if rec.LastScannedTranscriptCursor != 7 {
		t.Errorf("cursor = %d, want 7", rec.LastScannedTranscriptCursor)
	}
	if len(rec.BoundRepos) != 1 {
		t.Errorf("cursor write must not disturb bound repos: %+v", rec.BoundRepos)
	}
}

func TestRecordBinding_ConcurrentWritesAreSerialized(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()

	const workers = 10
	errs := make(chan error, workers)
	for range workers {
		go func() {
			errs <- RecordBinding(ctx, "sess-1", testMeta(), testEvidence("b", true))
		}()
	}
	for range workers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent RecordBinding failed: %v", err)
		}
	}

	rec, err := LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected one bound repo, got %+v", rec)
	}
	if got := rec.BoundRepos[0].EvidenceCount; got != workers {
		t.Errorf("evidence count = %d, want %d (lost update under concurrency)", got, workers)
	}
}
