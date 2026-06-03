package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Note: these tests use t.Setenv and/or t.Chdir, so t.Parallel() is not called.

func TestAppendStatusSnapshot_WritesLineKeyedByConversation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	payload := []byte(`{"conversation_id":"conv-1","agent_state":"working","context_window":{"total_input_tokens":1000,"total_output_tokens":50,"context_window_size":200000,"current_usage":{"input_tokens":900,"output_tokens":50,"cache_creation_input_tokens":100,"cache_read_input_tokens":800}}}`)

	if err := AppendStatusSnapshot(payload); err != nil {
		t.Fatalf("AppendStatusSnapshot: %v", err)
	}

	snaps, err := readStatusSnapshots("conv-1")
	if err != nil {
		t.Fatalf("readStatusSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snaps))
	}

	s := snaps[0]
	if s.ContextWindow.TotalInputTokens != 1000 {
		t.Errorf("TotalInputTokens = %d, want 1000", s.ContextWindow.TotalInputTokens)
	}
	if s.ContextWindow.TotalOutputTokens != 50 {
		t.Errorf("TotalOutputTokens = %d, want 50", s.ContextWindow.TotalOutputTokens)
	}
	if s.ContextWindow.CurrentUsage == nil {
		t.Fatal("CurrentUsage is nil")
	}
	if s.ContextWindow.CurrentUsage.CacheReadInputTokens != 800 {
		t.Errorf("CacheReadInputTokens = %d, want 800", s.ContextWindow.CurrentUsage.CacheReadInputTokens)
	}
	if s.Timestamp == "" {
		t.Error("Timestamp is empty")
	}
}

func TestAppendStatusSnapshot_DedupsUnchangedContextWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	// First payload
	p1 := []byte(`{"conversation_id":"conv-2","agent_state":"working","context_window":{"total_input_tokens":1000,"total_output_tokens":50}}`)
	// Second payload: same context_window, different agent_state
	p2 := []byte(`{"conversation_id":"conv-2","agent_state":"idle","context_window":{"total_input_tokens":1000,"total_output_tokens":50}}`)
	// Third payload: different context_window
	p3 := []byte(`{"conversation_id":"conv-2","agent_state":"working","context_window":{"total_input_tokens":2000,"total_output_tokens":100}}`)

	for _, p := range [][]byte{p1, p2, p3} {
		if err := AppendStatusSnapshot(p); err != nil {
			t.Fatalf("AppendStatusSnapshot: %v", err)
		}
	}

	snaps, err := readStatusSnapshots("conv-2")
	if err != nil {
		t.Fatalf("readStatusSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("got %d snapshots, want 2 (dedup should have skipped p2)", len(snaps))
	}
}

func TestAppendStatusSnapshot_IgnoresGarbageAndMissingFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	for _, payload := range []string{"not json", "{}", `{"conversation_id":"conv-3"}`} {
		if err := AppendStatusSnapshot([]byte(payload)); err != nil {
			t.Errorf("AppendStatusSnapshot(%q) returned error: %v", payload, err)
		}
	}

	// dir must be empty (no snapshot files written)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty dir, got %d entries", len(entries))
	}
}

func TestReadStatusSnapshots_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	// Write a file manually with valid / garbage / valid lines
	validLine1, err := json.Marshal(statusSnapshot{
		Timestamp:      "2026-01-01T00:00:00Z",
		ConversationID: "conv-4",
		ContextWindow:  statusContextWindow{TotalInputTokens: 10, TotalOutputTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	validLine2, err := json.Marshal(statusSnapshot{
		Timestamp:      "2026-01-01T00:01:00Z",
		ConversationID: "conv-4",
		ContextWindow:  statusContextWindow{TotalInputTokens: 20, TotalOutputTokens: 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(dir, "conv-4.jsonl")
	content := string(validLine1) + "\nGARBAGE LINE\n" + string(validLine2) + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	snaps, err := readStatusSnapshots("conv-4")
	if err != nil {
		t.Fatalf("readStatusSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("got %d snapshots, want 2 (malformed line skipped)", len(snaps))
	}
}

func TestReadStatusSnapshots_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	snaps, err := readStatusSnapshots("no-such-conv")
	if err != nil {
		t.Fatalf("readStatusSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("got %d snapshots, want 0", len(snaps))
	}
}

func BenchmarkAppendStatusSnapshot_GrownFile(b *testing.B) {
	dir := b.TempDir()
	b.Setenv(statusDirEnv, dir)

	// Seed 500 distinct snapshots
	for i := range 500 {
		payload, err := json.Marshal(map[string]any{
			"conversation_id": "bench-conv",
			"agent_state":     "working",
			"context_window": map[string]any{
				"total_input_tokens":  1000 + i,
				"total_output_tokens": 50 + i,
			},
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := AppendStatusSnapshot(payload); err != nil {
			b.Fatalf("seed %d: %v", i, err)
		}
	}

	// The duplicate payload matches the last seeded snapshot (dedup path)
	dupPayload, err := json.Marshal(map[string]any{
		"conversation_id": "bench-conv",
		"agent_state":     "working",
		"context_window": map[string]any{
			"total_input_tokens":  1000 + 499,
			"total_output_tokens": 50 + 499,
		},
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for range b.N {
		if err := AppendStatusSnapshot(dupPayload); err != nil {
			b.Fatal(err)
		}
	}
}

func TestAppendStatusSnapshot_PrunesStaleFilesOnCreate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	// Pre-seed a stale file for another conversation (mtime older than retention)
	stale := filepath.Join(dir, "old-conv.jsonl")
	if err := os.WriteFile(stale, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-statusRetention - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// And a fresh file for a third conversation that must survive
	fresh := filepath.Join(dir, "fresh-conv.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// First write for a new conversation triggers the prune
	payload := []byte(`{"conversation_id":"conv-new","context_window":{"total_input_tokens":1}}`)
	if err := AppendStatusSnapshot(payload); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file should be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh file must survive prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "conv-new.jsonl")); err != nil {
		t.Errorf("active file must exist: %v", err)
	}
}
