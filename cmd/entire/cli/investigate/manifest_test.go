package investigate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newManifest(runID, topic string, started time.Time, outcome string) LocalManifest {
	return LocalManifest{
		RunID:       runID,
		Topic:       topic,
		Slug:        SlugifyTopic(topic),
		StartingSHA: "deadbeefcafe",
		FindingsDoc: "/abs/findings-" + runID + ".md",
		Agents:      []string{"claude-code", "codex"},
		Outcome:     outcome,
		StancesByAgent: map[string]string{
			"claude-code": stanceApprove,
			"codex":       stanceRequestChanges,
		},
		StartedAt: started,
		EndedAt:   started.Add(10 * time.Minute),
	}
}

func TestLocalManifestStore_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)

	now := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	m := newManifest("abcdef012345", "Why is checkout flaky?", now, "quorum")

	if err := store.Write(context.Background(), m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	if got[0].RunID != m.RunID {
		t.Errorf("RunID = %q, want %q", got[0].RunID, m.RunID)
	}
	if got[0].Topic != m.Topic {
		t.Errorf("Topic = %q, want %q", got[0].Topic, m.Topic)
	}
	if got[0].Outcome != "quorum" {
		t.Errorf("Outcome = %q, want %q", got[0].Outcome, "quorum")
	}
	if got[0].StancesByAgent["claude-code"] != stanceApprove {
		t.Errorf("StancesByAgent[claude-code] = %q, want approve", got[0].StancesByAgent["claude-code"])
	}
	if !got[0].StartedAt.Equal(m.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got[0].StartedAt, m.StartedAt)
	}
	if len(got[0].Agents) != 2 || got[0].Agents[0] != "claude-code" {
		t.Errorf("Agents = %v", got[0].Agents)
	}
}

func TestLocalManifestStore_ListSortedNewestFirst(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)

	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)

	// Write out of order on purpose; sort order must come from StartedAt,
	// not write order.
	for _, m := range []LocalManifest{
		newManifest("aaaaaaaaaaaa", "older", t1, "stalled"),
		newManifest("cccccccccccc", "newest", t3, "quorum"),
		newManifest("bbbbbbbbbbbb", "middle", t2, "paused"),
	} {
		if err := store.Write(context.Background(), m); err != nil {
			t.Fatalf("Write %s: %v", m.RunID, err)
		}
	}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	wantOrder := []string{"cccccccccccc", "bbbbbbbbbbbb", "aaaaaaaaaaaa"}
	for i, want := range wantOrder {
		if got[i].RunID != want {
			t.Errorf("List[%d].RunID = %q, want %q", i, got[i].RunID, want)
		}
	}
}

func TestLocalManifestStore_FindByRunID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	m := newManifest("abcdef012345", "Why slow?", now, "quorum")
	if err := store.Write(context.Background(), m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		got, ok, err := store.FindByRunID(context.Background(), "abcdef012345")
		if err != nil {
			t.Fatalf("FindByRunID: %v", err)
		}
		if !ok {
			t.Fatal("FindByRunID returned ok=false for an existing manifest")
		}
		if got.Topic != "Why slow?" {
			t.Errorf("Topic = %q, want %q", got.Topic, "Why slow?")
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		got, ok, err := store.FindByRunID(context.Background(), "ffffffffffff")
		if err != nil {
			t.Fatalf("FindByRunID (missing): %v", err)
		}
		if ok {
			t.Error("FindByRunID returned ok=true for a missing run ID")
		}
		if got.RunID != "" {
			t.Errorf("returned manifest has RunID = %q, want empty", got.RunID)
		}
	})

	t.Run("invalid id", func(t *testing.T) {
		t.Parallel()
		_, _, err := store.FindByRunID(context.Background(), "not-hex")
		if err == nil {
			t.Error("expected error for invalid run ID")
		}
	})

	t.Run("empty id", func(t *testing.T) {
		t.Parallel()
		_, _, err := store.FindByRunID(context.Background(), "")
		if err == nil {
			t.Error("expected error for empty run ID")
		}
	})
}

func TestLocalManifestStore_FindingsContentRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)

	now := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	m := newManifest("abcdef012345", "Why is checkout flaky?", now, "quorum")
	m.FindingsContent = "# Findings\n\nThe checkout race only fires on macOS.\n"

	if err := store.Write(context.Background(), m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, ok, err := store.FindByRunID(context.Background(), m.RunID)
	if err != nil {
		t.Fatalf("FindByRunID: %v", err)
	}
	if !ok {
		t.Fatal("FindByRunID returned ok=false for an existing manifest")
	}
	if got.FindingsContent != m.FindingsContent {
		t.Errorf("FindingsContent = %q, want %q", got.FindingsContent, m.FindingsContent)
	}
	// The on-disk path should still be carried alongside the embedded
	// content — readers may want to display it for context, and runs
	// that did NOT terminate would store the path with empty content.
	if got.FindingsDoc != m.FindingsDoc {
		t.Errorf("FindingsDoc = %q, want %q", got.FindingsDoc, m.FindingsDoc)
	}
}

func TestLocalManifestStore_MissingDirReturnsEmpty(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")
	store := NewLocalManifestStoreWithDir(dir)

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List len = %d, want 0", len(got))
	}
}

// TestLocalManifestStore_ListSkipsInvalidRunID verifies that a planted
// manifest whose run_id is a path-traversal payload is dropped by List, so it
// can never flow through clean → RunDir → os.RemoveAll. Write() rejects such
// ids, so the malicious manifest is written raw to disk to simulate tampering
// (precondition: attacker has .git write access).
func TestLocalManifestStore_ListSkipsInvalidRunID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)

	good := newManifest("abcdef012345", "legit", time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC), "quorum")
	if err := store.Write(context.Background(), good); err != nil {
		t.Fatalf("Write good manifest: %v", err)
	}

	evil := []byte(`{"run_id":"../../../etc","topic":"evil","started_at":"2026-05-08T12:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(dir, "evil.json"), evil, 0o600); err != nil {
		t.Fatalf("write evil manifest: %v", err)
	}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1 (the invalid run_id must be skipped)", len(got))
	}
	if got[0].RunID != "abcdef012345" {
		t.Errorf("RunID = %q, want %q", got[0].RunID, "abcdef012345")
	}
}

// TestResolveByRunID_IgnoresInvalidRunID verifies defense-in-depth: even if a
// caller passes an unfiltered list, ResolveByRunID never matches (and so never
// resolves a delete target to) a manifest whose RunID is invalid.
func TestResolveByRunID_IgnoresInvalidRunID(t *testing.T) {
	t.Parallel()

	manifests := []LocalManifest{
		{RunID: "../../../etc"},
		{RunID: "abcdef012345"},
	}

	// A prefix query that textually matches the traversal entry must not
	// resolve to it.
	if got, err := ResolveByRunID(manifests, ".."); err == nil {
		t.Errorf("ResolveByRunID(\"..\") resolved to %+v; want error (invalid entry must be ignored)", got)
	}

	// The valid entry still resolves.
	got, err := ResolveByRunID(manifests, "abcdef012345")
	if err != nil {
		t.Fatalf("ResolveByRunID(valid): %v", err)
	}
	if len(got) != 1 || got[0].RunID != "abcdef012345" {
		t.Errorf("ResolveByRunID(valid) = %+v, want single abcdef012345", got)
	}
}
