package devin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sampleTranscript mirrors the live-captured ATIF-v1.7 shape (see AGENT.md):
// system/user/agent steps, tool_calls on agent steps, per-step metrics.
const sampleTranscript = `{
  "schema_version": "ATIF-v1.7",
  "session_id": "almond-cylinder",
  "agent": {"name": "devin", "version": "3000.2.17", "model_name": "SWE-1.7", "extra": {"backend": "Windsurf"}},
  "steps": [
    {"step_id": 1, "timestamp": "2026-07-27T11:59:51Z", "source": "system", "message": "system prompt"},
    {"step_id": 2, "timestamp": "2026-07-27T11:59:51Z", "source": "user", "message": "Append a line to hello.txt"},
    {"step_id": 3, "timestamp": "2026-07-27T12:01:40Z", "source": "agent", "message": "", "model_name": "SWE-1.7",
     "tool_calls": [{"tool_call_id": "read_0", "function_name": "read", "arguments": {"file_path": "/repo/hello.txt"}}],
     "metrics": {"prompt_tokens": 12431, "completion_tokens": 393, "cached_tokens": 10917}},
    {"step_id": 4, "timestamp": "2026-07-27T12:01:45Z", "source": "agent", "message": "", "model_name": "SWE-1.7",
     "tool_calls": [{"tool_call_id": "edit_0", "function_name": "edit", "arguments": {"file_path": "/repo/hello.txt", "old_string": "a", "new_string": "b"}},
                    {"tool_call_id": "write_0", "function_name": "write", "arguments": {"file_path": "/repo/new.txt", "content": "x"}}],
     "metrics": {"prompt_tokens": 12896, "completion_tokens": 254, "cached_tokens": 12430}},
    {"step_id": 5, "timestamp": "2026-07-27T12:01:50Z", "source": "agent", "message": "Done.", "model_name": "SWE-1.7",
     "metrics": {"prompt_tokens": 13231, "completion_tokens": 49, "cached_tokens": 12895}}
  ],
  "final_metrics": {"total_prompt_tokens": 38558, "total_completion_tokens": 696, "total_cached_tokens": 36242, "total_steps": 5}
}`

func writeSampleTranscript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "almond-cylinder.json")
	if err := os.WriteFile(path, []byte(sampleTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}

	path := writeSampleTranscript(t)
	pos, err := d.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition: %v", err)
	}
	if pos != 5 {
		t.Errorf("position = %d, want 5", pos)
	}

	if pos, err := d.GetTranscriptPosition(filepath.Join(t.TempDir(), "missing.json")); err != nil || pos != 0 {
		t.Errorf("missing file: pos = %d, err = %v; want 0, nil", pos, err)
	}
	if pos, err := d.GetTranscriptPosition(""); err != nil || pos != 0 {
		t.Errorf("empty path: pos = %d, err = %v; want 0, nil", pos, err)
	}
}

func TestExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	path := writeSampleTranscript(t)

	// From the start: edit + write files, deduplicated; read is not a modification.
	files, pos, err := d.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset: %v", err)
	}
	if pos != 5 {
		t.Errorf("position = %d, want 5", pos)
	}
	want := []string{"/repo/hello.txt", "/repo/new.txt"}
	if len(files) != len(want) || files[0] != want[0] || files[1] != want[1] {
		t.Errorf("files = %v, want %v", files, want)
	}

	// From offset 4: only the final step, which has no tool calls.
	files, _, err = d.ExtractModifiedFilesFromOffset(path, 4)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset offset 4: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("files from offset 4 = %v, want none", files)
	}
}

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}

	usage, err := d.CalculateTokenUsage([]byte(sampleTranscript), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	// InputTokens = sum(prompt - cached): (12431-10917)+(12896-12430)+(13231-12895) = 1514+466+336
	if usage.InputTokens != 2316 {
		t.Errorf("InputTokens = %d, want 2316", usage.InputTokens)
	}
	if usage.CacheReadTokens != 10917+12430+12895 {
		t.Errorf("CacheReadTokens = %d, want %d", usage.CacheReadTokens, 10917+12430+12895)
	}
	if usage.OutputTokens != 393+254+49 {
		t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, 393+254+49)
	}
	if usage.APICallCount != 3 {
		t.Errorf("APICallCount = %d, want 3", usage.APICallCount)
	}

	// Offset past the first agent step scopes the sums.
	usage, err = d.CalculateTokenUsage([]byte(sampleTranscript), 3)
	if err != nil {
		t.Fatalf("CalculateTokenUsage offset 3: %v", err)
	}
	if usage.APICallCount != 2 || usage.OutputTokens != 254+49 {
		t.Errorf("offset usage = %+v, want 2 calls, %d output", usage, 254+49)
	}
}

func TestExtractModel(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}

	model, err := d.ExtractModel([]byte(sampleTranscript))
	if err != nil {
		t.Fatalf("ExtractModel: %v", err)
	}
	if model != "SWE-1.7" {
		t.Errorf("model = %q, want SWE-1.7", model)
	}

	// Falls back to the agent block when steps carry no model.
	noStepModel := `{"schema_version":"ATIF-v1.7","session_id":"x","agent":{"model_name":"SWE-2"},"steps":[{"step_id":1,"source":"user","message":"hi"}]}`
	model, err = d.ExtractModel([]byte(noStepModel))
	if err != nil {
		t.Fatalf("ExtractModel fallback: %v", err)
	}
	if model != "SWE-2" {
		t.Errorf("fallback model = %q, want SWE-2", model)
	}
}

func TestChunkAndReassembleTranscript(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	ctx := context.Background()

	// Small maxSize forces one step per chunk.
	chunks, err := d.ChunkTranscript(ctx, []byte(sampleTranscript), 900)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	reassembled, err := d.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript: %v", err)
	}
	rt, err := parseTranscript(reassembled)
	if err != nil {
		t.Fatalf("parse reassembled: %v", err)
	}
	if len(rt.Steps) != 5 {
		t.Errorf("reassembled steps = %d, want 5", len(rt.Steps))
	}
	if rt.SessionID != "almond-cylinder" || rt.SchemaVersion != "ATIF-v1.7" {
		t.Errorf("envelope lost: %q %q", rt.SessionID, rt.SchemaVersion)
	}

	// Reassembled transcript must still yield the same analysis results.
	files, err := ExtractModifiedFiles(reassembled)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("files after reassembly = %v, want 2 entries", files)
	}
}

func TestChunkTranscript_FitsInOneChunk(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	chunks, err := d.ChunkTranscript(context.Background(), []byte(sampleTranscript), 1<<20)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("chunks = %d, want 1", len(chunks))
	}
	if string(chunks[0]) != sampleTranscript {
		t.Error("single chunk should be the original content, unmodified")
	}
}

func TestReassembleTranscript_Empty(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	if _, err := d.ReassembleTranscript(nil); err == nil {
		t.Error("expected error for empty chunks")
	}
}

func TestPrepareTranscript_FastPaths(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	ctx := context.Background()

	// Fresh file: returns immediately.
	fresh := writeSampleTranscript(t)
	start := time.Now()
	if err := d.PrepareTranscript(ctx, fresh); err != nil {
		t.Fatalf("PrepareTranscript fresh: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("fresh file took %v, want immediate return", elapsed)
	}

	// Stale file (old mtime): returns immediately, no flush is coming.
	stale := writeSampleTranscript(t)
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := d.PrepareTranscript(ctx, stale); err != nil {
		t.Fatalf("PrepareTranscript stale: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("stale file took %v, want immediate return", elapsed)
	}

	// Empty ref: no-op.
	if err := d.PrepareTranscript(ctx, ""); err != nil {
		t.Fatalf("PrepareTranscript empty ref: %v", err)
	}
}

func TestPrepareTranscript_MaterializesStubWhenMissing(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	path := filepath.Join(t.TempDir(), "brisk-otter.json")

	if err := d.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub transcript was not materialized: %v", err)
	}
	stub, err := parseTranscript(data)
	if err != nil {
		t.Fatalf("stub is not valid ATIF: %v", err)
	}
	if stub.SessionID != "brisk-otter" {
		t.Errorf("stub session_id = %q, want brisk-otter", stub.SessionID)
	}
	if len(stub.Steps) != 0 {
		t.Errorf("stub steps = %d, want 0", len(stub.Steps))
	}

	// The stub must satisfy the analyzer surface too.
	pos, err := d.GetTranscriptPosition(path)
	if err != nil || pos != 0 {
		t.Errorf("stub position = %d, %v; want 0, nil", pos, err)
	}
}

func TestPrepareTranscript_WaitsForFlush(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	path := filepath.Join(t.TempDir(), "late.json")

	go func() {
		time.Sleep(150 * time.Millisecond)
		if err := os.WriteFile(path, []byte(sampleTranscript), 0o600); err != nil {
			panic(err)
		}
	}()

	start := time.Now()
	if err := d.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	elapsed := time.Since(start)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("file was not written by the goroutine")
	}
	if elapsed >= 2*time.Second {
		t.Errorf("waited full timeout (%v) despite flush landing at 150ms", elapsed)
	}
}
