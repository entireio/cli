package antigravity

import (
	"context"
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

// writeSnapshotFixture writes the given snapshots as JSONL to the snapshot file
// for conversationID, using the statusDirEnv override already set by the test.
func writeSnapshotFixture(t *testing.T, conversationID string, snaps []statusSnapshot) {
	t.Helper()
	path, err := statusFilePath(conversationID)
	if err != nil {
		t.Fatalf("statusFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var buf []byte
	for _, s := range snaps {
		line, marshalErr := json.Marshal(s)
		if marshalErr != nil {
			t.Fatalf("marshal snapshot: %v", marshalErr)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func TestCalculateTokenUsageSince_DeltaFromBaseline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	snaps := []statusSnapshot{
		{
			Timestamp:      "2026-06-03T10:00:00.000000000Z",
			ConversationID: "c1",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  1000,
				TotalOutputTokens: 100,
				CurrentUsage:      &statusCurrentUsage{CacheCreationInputTokens: 200, CacheReadInputTokens: 700},
			},
		},
		{
			Timestamp:      "2026-06-03T10:05:00.000000000Z",
			ConversationID: "c1",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  3000,
				TotalOutputTokens: 250,
				CurrentUsage:      &statusCurrentUsage{CacheCreationInputTokens: 50, CacheReadInputTokens: 1900},
			},
		},
		{
			Timestamp:      "2026-06-03T10:06:00.000000000Z",
			ConversationID: "c1",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  4500,
				TotalOutputTokens: 400,
				CurrentUsage:      &statusCurrentUsage{CacheCreationInputTokens: 0, CacheReadInputTokens: 1500},
			},
		},
	}
	writeSnapshotFixture(t, "c1", snaps)

	baseline, err := json.Marshal(snaps[0])
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}

	a := &AntigravityAgent{}
	usage, err := a.CalculateTokenUsageSince(context.Background(), "c1", baseline)
	if err != nil {
		t.Fatalf("CalculateTokenUsageSince: %v", err)
	}
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 3500 {
		t.Errorf("InputTokens = %d, want 3500", usage.InputTokens)
	}
	if usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
	}
	if usage.CacheCreationTokens != 50 {
		t.Errorf("CacheCreationTokens = %d, want 50", usage.CacheCreationTokens)
	}
	if usage.CacheReadTokens != 3400 {
		t.Errorf("CacheReadTokens = %d, want 3400", usage.CacheReadTokens)
	}
	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", usage.APICallCount)
	}
}

func TestCalculateTokenUsageSince_NilBaselineCountsFromZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	snaps := []statusSnapshot{
		{
			Timestamp:      "2026-06-03T10:00:00.000000000Z",
			ConversationID: "c2",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  1000,
				TotalOutputTokens: 100,
				CurrentUsage:      &statusCurrentUsage{CacheReadInputTokens: 700},
			},
		},
	}
	writeSnapshotFixture(t, "c2", snaps)

	a := &AntigravityAgent{}
	usage, err := a.CalculateTokenUsageSince(context.Background(), "c2", nil)
	if err != nil {
		t.Fatalf("CalculateTokenUsageSince: %v", err)
	}
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", usage.InputTokens)
	}
	if usage.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100", usage.OutputTokens)
	}
}

func TestCalculateTokenUsageSince_NoDataReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	a := &AntigravityAgent{}
	usage, err := a.CalculateTokenUsageSince(context.Background(), "missing-conv", nil)
	if err != nil {
		t.Fatalf("CalculateTokenUsageSince: %v", err)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil", usage)
	}
}

func TestSnapshotTokenBaseline_ReturnsLatestSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	snaps := []statusSnapshot{
		{
			Timestamp:      "2026-06-03T10:00:00.000000000Z",
			ConversationID: "c3",
			ContextWindow:  statusContextWindow{TotalInputTokens: 1000},
		},
		{
			Timestamp:      "2026-06-03T10:05:00.000000000Z",
			ConversationID: "c3",
			ContextWindow:  statusContextWindow{TotalInputTokens: 2000},
		},
	}
	writeSnapshotFixture(t, "c3", snaps)

	a := &AntigravityAgent{}
	baseline, err := a.SnapshotTokenBaseline(context.Background(), "c3")
	if err != nil {
		t.Fatalf("SnapshotTokenBaseline: %v", err)
	}
	if len(baseline) == 0 {
		t.Fatal("baseline is empty")
	}
	var snap statusSnapshot
	if err := json.Unmarshal(baseline, &snap); err != nil {
		t.Fatalf("unmarshal baseline: %v", err)
	}
	if snap.ContextWindow.TotalInputTokens != 2000 {
		t.Errorf("baseline TotalInputTokens = %d, want 2000", snap.ContextWindow.TotalInputTokens)
	}
}

func TestSnapshotTokenBaseline_EmptyStoreReturnsNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	a := &AntigravityAgent{}
	baseline, err := a.SnapshotTokenBaseline(context.Background(), "no-such-conv")
	if err != nil {
		t.Fatalf("SnapshotTokenBaseline: %v", err)
	}
	if baseline != nil {
		t.Errorf("baseline = %v, want nil", baseline)
	}
}

func TestCalculateTokenUsageSince_ClampsWhenTotalsGoBackwards(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	// Simulate conversation-id reuse where cumulative totals reset lower than the baseline.
	snaps := []statusSnapshot{
		{
			Timestamp:      "2026-06-03T10:05:00.000000000Z",
			ConversationID: "c1",
			ContextWindow:  statusContextWindow{TotalInputTokens: 500, TotalOutputTokens: 30},
		},
	}
	writeSnapshotFixture(t, "c1", snaps)

	// Baseline has higher totals than the latest snapshot — totals went backwards.
	baseSnap := statusSnapshot{
		Timestamp:     "2026-06-03T10:00:00.000000000Z",
		ContextWindow: statusContextWindow{TotalInputTokens: 5000, TotalOutputTokens: 400},
	}
	baseline, err := json.Marshal(baseSnap)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}

	a := &AntigravityAgent{}
	usage, err := a.CalculateTokenUsageSince(context.Background(), "c1", baseline)
	if err != nil {
		t.Fatalf("CalculateTokenUsageSince: %v", err)
	}
	// The single line has no current_usage, so once max(0, ...) clamps the
	// negative input/output deltas to zero, every field is zero and the method
	// returns (nil, nil) — the all-zero "nothing observed" path. If the clamp
	// were removed, the negative deltas would make usage non-nil, so asserting
	// nil here pins the clamp behavior directly.
	if usage != nil {
		t.Errorf("usage = %+v, want nil (negative deltas clamped to zero -> all-zero -> nil)", usage)
	}
}

func TestCalculateTokenUsageSince_UnparseableBaselineCountsAllLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	snaps := []statusSnapshot{
		{
			Timestamp:      "2026-06-03T10:00:00.000000000Z",
			ConversationID: "c1",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  1000,
				TotalOutputTokens: 100,
				CurrentUsage:      &statusCurrentUsage{CacheReadInputTokens: 700},
			},
		},
		{
			Timestamp:      "2026-06-03T10:05:00.000000000Z",
			ConversationID: "c1",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  3000,
				TotalOutputTokens: 250,
				CurrentUsage:      &statusCurrentUsage{CacheReadInputTokens: 900},
			},
		},
	}
	writeSnapshotFixture(t, "c1", snaps)

	// Baseline with an unparseable timestamp — documents accepted degradation:
	// input/output stay exact via the totals subtraction, but cache/apicall
	// counts cover all lines rather than only post-baseline lines.
	baseline := json.RawMessage(`{"ts":"not-a-timestamp","context_window":{"total_input_tokens":1000,"total_output_tokens":100}}`)

	a := &AntigravityAgent{}
	usage, err := a.CalculateTokenUsageSince(context.Background(), "c1", baseline)
	if err != nil {
		t.Fatalf("CalculateTokenUsageSince: %v", err)
	}
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 2000 {
		t.Errorf("InputTokens = %d, want 2000 (3000-1000)", usage.InputTokens)
	}
	if usage.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150 (250-100)", usage.OutputTokens)
	}
	// Both lines are counted because the baseline timestamp didn't parse.
	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2 (all lines counted)", usage.APICallCount)
	}
	if usage.CacheReadTokens != 1600 {
		t.Errorf("CacheReadTokens = %d, want 1600 (700+900)", usage.CacheReadTokens)
	}
}

// TestAppendStatusSnapshot_CurrentUsageChangeIsNotDeduped proves that two
// payloads with IDENTICAL total_input_tokens/total_output_tokens but DIFFERENT
// current_usage are NOT deduped: both must be persisted. The delta calculation
// sums per-line cache fields from current_usage, so collapsing two lines that
// differ only in current_usage would silently drop cache accounting.
func TestAppendStatusSnapshot_CurrentUsageChangeIsNotDeduped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	// Same totals {1000,100}, different current_usage cache_read.
	a := []byte(`{"conversation_id":"conv-cu","agent_state":"working","context_window":{"total_input_tokens":1000,"total_output_tokens":100,"current_usage":{"cache_read_input_tokens":700}}}`)
	b := []byte(`{"conversation_id":"conv-cu","agent_state":"working","context_window":{"total_input_tokens":1000,"total_output_tokens":100,"current_usage":{"cache_read_input_tokens":900}}}`)

	for _, p := range [][]byte{a, b} {
		if err := AppendStatusSnapshot(p); err != nil {
			t.Fatalf("AppendStatusSnapshot: %v", err)
		}
	}

	snaps, err := readStatusSnapshots("conv-cu")
	if err != nil {
		t.Fatalf("readStatusSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2 (current_usage change must not be deduped)", len(snaps))
	}
}

// TestCalculateTokenUsageSince_NoDoubleCountWhenBaselineIsLatest proves the
// load-bearing strictly-.After filter prevents double-counting across turns.
// Simulating turn N+1: use turn N's LATEST snapshot as the baseline and append
// nothing new. CalculateTokenUsageSince must return (nil, nil) — zero delta,
// APICallCount 0 — because no snapshot is strictly after the baseline timestamp
// and totals − baseline = 0.
func TestCalculateTokenUsageSince_NoDoubleCountWhenBaselineIsLatest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(statusDirEnv, dir)

	snaps := []statusSnapshot{
		{
			Timestamp:      "2026-06-03T10:00:00.000000000Z",
			ConversationID: "c1",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  1000,
				TotalOutputTokens: 100,
				CurrentUsage:      &statusCurrentUsage{CacheCreationInputTokens: 200, CacheReadInputTokens: 700},
			},
		},
		{
			Timestamp:      "2026-06-03T10:05:00.000000000Z",
			ConversationID: "c1",
			ContextWindow: statusContextWindow{
				TotalInputTokens:  3000,
				TotalOutputTokens: 250,
				CurrentUsage:      &statusCurrentUsage{CacheCreationInputTokens: 50, CacheReadInputTokens: 1900},
			},
		},
	}
	writeSnapshotFixture(t, "c1", snaps)

	a := &AntigravityAgent{}

	// Turn N: full usage from nil baseline.
	full, err := a.CalculateTokenUsageSince(context.Background(), "c1", nil)
	if err != nil {
		t.Fatalf("CalculateTokenUsageSince (full): %v", err)
	}
	if full == nil || full.InputTokens != 3000 {
		t.Fatalf("full usage = %+v, want InputTokens 3000", full)
	}

	// Capture the latest snapshot (T2 line) exactly as the lifecycle does.
	baseline, err := a.SnapshotTokenBaseline(context.Background(), "c1")
	if err != nil {
		t.Fatalf("SnapshotTokenBaseline: %v", err)
	}
	if len(baseline) == 0 {
		t.Fatal("baseline is empty")
	}

	// Turn N+1: nothing new appended. Delta from the latest baseline is zero.
	usage, err := a.CalculateTokenUsageSince(context.Background(), "c1", baseline)
	if err != nil {
		t.Fatalf("CalculateTokenUsageSince (delta): %v", err)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil (no snapshot strictly after baseline; zero delta)", usage)
	}
}
