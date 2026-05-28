package investigate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateStore_SaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	now := time.Now().UTC().Truncate(time.Second)
	st := &RunState{
		RunID:           "abcdef012345",
		Topic:           "Why is checkout flaky?",
		Agents:          []string{"claude-code", "codex"},
		MaxTurns:        3,
		Quorum:          2,
		CompletedRounds: 1,
		Turn:            2,
		NextAgentIdx:    1,
		Stances: []TurnStance{
			{Round: 1, Turn: 1, Agent: "claude-code", Stance: "approve", PlanChanged: true},
		},
		FindingsDoc: "/tmp/findings.md",
		StartingSHA: "deadbeef",
		StartedAt:   now,
		UpdatedAt:   now,
		PendingTurn: &PendingTurn{Stance: "approve", Note: "all clear"},
	}

	if err := store.Save(context.Background(), st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load(context.Background(), st.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil for an existing run")
	}
	if got.RunID != st.RunID {
		t.Errorf("RunID = %q, want %q", got.RunID, st.RunID)
	}
	if got.Topic != st.Topic {
		t.Errorf("Topic = %q, want %q", got.Topic, st.Topic)
	}
	if len(got.Agents) != len(st.Agents) || got.Agents[0] != st.Agents[0] || got.Agents[1] != st.Agents[1] {
		t.Errorf("Agents = %v", got.Agents)
	}
	if got.MaxTurns != st.MaxTurns {
		t.Errorf("MaxTurns = %d", got.MaxTurns)
	}
	if got.Quorum != st.Quorum {
		t.Errorf("Quorum = %d", got.Quorum)
	}
	if got.CompletedRounds != st.CompletedRounds || got.Turn != st.Turn || got.NextAgentIdx != st.NextAgentIdx {
		t.Errorf("CompletedRounds/Turn/NextAgentIdx = %d/%d/%d", got.CompletedRounds, got.Turn, got.NextAgentIdx)
	}
	if len(got.Stances) != 1 || got.Stances[0].Stance != "approve" {
		t.Errorf("Stances = %+v", got.Stances)
	}
	if got.FindingsDoc != st.FindingsDoc {
		t.Errorf("FindingsDoc = %q, want %q", got.FindingsDoc, st.FindingsDoc)
	}
	if !got.StartedAt.Equal(st.StartedAt) || !got.UpdatedAt.Equal(st.UpdatedAt) {
		t.Errorf("timestamps mismatch")
	}
	if got.PendingTurn == nil {
		t.Errorf("PendingTurn = nil, want round-tripped pending turn")
	} else if got.PendingTurn.Stance != "approve" || got.PendingTurn.Note != "all clear" {
		t.Errorf("PendingTurn = %+v, want approve/all clear", got.PendingTurn)
	}
}

func TestStateStore_RunDirComposition(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	const runID = "abcdef012345"

	got := store.RunDir(runID)
	want := filepath.Join(dir, runID)
	if got != want {
		t.Errorf("RunDir = %q, want %q", got, want)
	}
}

func TestStateStore_SaveCreatesPerRunDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	st := &RunState{
		RunID:       "abcdef012345",
		Topic:       "topic",
		StartingSHA: "sha",
		StartedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	statePath := filepath.Join(dir, st.RunID, "state.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("expected state file at %s, got: %v", statePath, err)
	}
}

func TestStateStore_LoadMissingReturnsNilNil(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	got, err := store.Load(context.Background(), "abcdef012345")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing run, got %+v", got)
	}
}

func TestStateStore_LoadMissingDirectoryReturnsNilNil(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	store := NewStateStoreWithDir(dir)
	got, err := store.Load(context.Background(), "abcdef012345")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing dir, got %+v", got)
	}
}

func TestStateStore_List(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	now := time.Now().UTC()
	for _, runID := range []string{"abcdef012345", "0123456789ab"} {
		if err := store.Save(context.Background(), &RunState{
			RunID:       runID,
			Topic:       "topic",
			StartingSHA: "sha",
			StartedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("Save(%s): %v", runID, err)
		}
	}

	// A non-run sibling in the directory (e.g. the manifests/ subdir or a
	// stray file) must be ignored, not crash List.
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0o750); err != nil {
		t.Fatalf("mkdir manifests sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "garbage.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("List() returned %d entries, want 2", len(got))
	}
	seen := make(map[string]bool)
	for _, st := range got {
		seen[st.RunID] = true
	}
	if !seen["abcdef012345"] || !seen["0123456789ab"] {
		t.Errorf("missing run IDs: %+v", seen)
	}
}

func TestStateStore_ListEmptyDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "missing")
	store := NewStateStoreWithDir(dir)
	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List on missing dir should return empty, got %+v", got)
	}
}

func TestStateStore_Clear(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	now := time.Now().UTC()
	st := &RunState{
		RunID:       "abcdef012345",
		Topic:       "topic",
		StartingSHA: "sha",
		StartedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Clear(context.Background(), st.RunID); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	// Idempotent — clearing a missing run is a no-op.
	if err := store.Clear(context.Background(), st.RunID); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
	// And Load now returns (nil, nil).
	got, err := store.Load(context.Background(), st.RunID)
	if err != nil {
		t.Fatalf("Load after clear: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after clear, got %+v", got)
	}
}

// TestValidateRunID covers the path-traversal-resistant input validation:
// only 12 lowercase hex characters are allowed.
func TestValidateRunID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		runID   string
		wantErr bool
	}{
		{"valid", "abcdef012345", false},
		{"valid_zeros", "000000000000", false},
		{"empty", "", true},
		{"too_short", "abc", true},
		{"too_long", "abcdef0123456", true},
		{"uppercase", "ABCDEF012345", true},
		{"non_hex", "abcdefghijkl", true},
		{"path_traversal", "../etc/passw", true},
		{"slash", "abc/ef012345", true},
		{"backslash", `abc\ef012345`, true},
		{"dot_dot", "............", true},
		{"with_space", "abcdef 12345", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRunID(tc.runID)
			if tc.wantErr && err == nil {
				t.Errorf("validateRunID(%q) = nil, want error", tc.runID)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateRunID(%q) = %v, want nil", tc.runID, err)
			}
		})
	}
}

func TestStateStore_RejectsInvalidRunID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	ctx := context.Background()
	now := time.Now().UTC()

	bad := []string{"", "../oops", "ABCDEF012345", "abc/def", "short"}
	for _, runID := range bad {
		st := &RunState{
			RunID:       runID,
			Topic:       "topic",
			StartingSHA: "sha",
			StartedAt:   now,
			UpdatedAt:   now,
		}
		if err := store.Save(ctx, st); err == nil {
			t.Errorf("Save(%q): expected error, got nil", runID)
		}
		if _, err := store.Load(ctx, runID); err == nil {
			t.Errorf("Load(%q): expected error, got nil", runID)
		}
		if err := store.Clear(ctx, runID); err == nil {
			t.Errorf("Clear(%q): expected error, got nil", runID)
		}
	}
}

// TestStateStore_SaveDoesNotLeaveTempFiles checks that the atomic temp+rename
// pattern doesn't leave .tmp files behind on success. A leaked .tmp would
// later trip up List or Clear behavior.
func TestStateStore_SaveDoesNotLeaveTempFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)
	now := time.Now().UTC()
	st := &RunState{
		RunID:       "abcdef012345",
		Topic:       "topic",
		StartingSHA: "sha",
		StartedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	runDir := filepath.Join(dir, st.RunID)
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		// CreateTemp("..", "state.json.*.tmp") yields names containing a
		// random infix between "state.json." and a trailing ".tmp"; pin
		// both endpoints so an unrelated future filename can't slip by.
		if filepath.Ext(name) == ".tmp" {
			t.Errorf("found leftover temp file: %s", name)
		}
	}
}
