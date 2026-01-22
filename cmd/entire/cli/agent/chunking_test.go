package agent

import (
	"strings"
	"testing"
)

func TestChunkJSONL_SmallContent(t *testing.T) {
	// Small transcript should not be chunked
	content := []byte(`{"type":"human","message":"hello"}
{"type":"assistant","message":"hi"}`)

	chunks, err := ChunkJSONL(content, MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkJSONL error: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk, got %d", len(chunks))
	}
	if string(chunks[0]) != string(content) {
		t.Errorf("Chunk content mismatch")
	}
}

func TestChunkJSONL_LargeContent(t *testing.T) {
	// Create a transcript larger than MaxChunkSize
	var lines []string
	lineContent := `{"type":"human","message":"` + strings.Repeat("x", 1000) + `"}`
	// Calculate how many lines needed to exceed MaxChunkSize
	linesNeeded := (MaxChunkSize / len(lineContent)) + 100 // Extra to ensure multiple chunks

	for range linesNeeded {
		lines = append(lines, lineContent)
	}
	content := []byte(strings.Join(lines, "\n"))

	chunks, err := ChunkJSONL(content, MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkJSONL error: %v", err)
	}
	if len(chunks) < 2 {
		t.Errorf("Expected at least 2 chunks for large content, got %d", len(chunks))
	}

	// Verify each chunk is under MaxChunkSize
	for i, chunk := range chunks {
		if len(chunk) > MaxChunkSize {
			t.Errorf("Chunk %d exceeds MaxChunkSize: %d > %d", i, len(chunk), MaxChunkSize)
		}
	}

	// Verify reassembly
	reassembled := ReassembleJSONL(chunks)
	if string(reassembled) != string(content) {
		t.Errorf("Reassembled content doesn't match original")
	}
}

func TestChunkTranscript_SmallContent_NoAgent(t *testing.T) {
	// Without a registered agent, ChunkTranscript falls back to JSONL
	content := []byte(`{"type":"human","message":"hello"}`)

	chunks, err := ChunkTranscript(content, "")
	if err != nil {
		t.Fatalf("ChunkTranscript error: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk, got %d", len(chunks))
	}
}

func TestChunkFileName(t *testing.T) {
	tests := []struct {
		baseName string
		index    int
		expected string
	}{
		{"full.jsonl", 0, "full.jsonl"},
		{"full.jsonl", 1, "full.jsonl.001"},
		{"full.jsonl", 2, "full.jsonl.002"},
		{"full.jsonl", 10, "full.jsonl.010"},
		{"full.jsonl", 100, "full.jsonl.100"},
	}

	for _, tt := range tests {
		result := ChunkFileName(tt.baseName, tt.index)
		if result != tt.expected {
			t.Errorf("ChunkFileName(%q, %d) = %q, want %q", tt.baseName, tt.index, result, tt.expected)
		}
	}
}

func TestParseChunkIndex(t *testing.T) {
	tests := []struct {
		filename string
		baseName string
		expected int
	}{
		{"full.jsonl", "full.jsonl", 0},
		{"full.jsonl.001", "full.jsonl", 1},
		{"full.jsonl.002", "full.jsonl", 2},
		{"full.jsonl.010", "full.jsonl", 10},
		{"full.jsonl.100", "full.jsonl", 100},
		{"other.txt", "full.jsonl", -1},
		{"full.jsonl.abc", "full.jsonl", -1},
	}

	for _, tt := range tests {
		result := ParseChunkIndex(tt.filename, tt.baseName)
		if result != tt.expected {
			t.Errorf("ParseChunkIndex(%q, %q) = %d, want %d", tt.filename, tt.baseName, result, tt.expected)
		}
	}
}

func TestSortChunkFiles(t *testing.T) {
	files := []string{"full.jsonl.003", "full.jsonl.001", "full.jsonl", "full.jsonl.002"}
	expected := []string{"full.jsonl", "full.jsonl.001", "full.jsonl.002", "full.jsonl.003"}

	sorted := SortChunkFiles(files, "full.jsonl")

	if len(sorted) != len(expected) {
		t.Fatalf("Length mismatch: got %d, want %d", len(sorted), len(expected))
	}
	for i, f := range sorted {
		if f != expected[i] {
			t.Errorf("Index %d: got %q, want %q", i, f, expected[i])
		}
	}
}

func TestReassembleJSONL_SingleChunk(t *testing.T) {
	content := []byte(`{"type":"human","message":"hello"}`)
	chunks := [][]byte{content}

	result := ReassembleJSONL(chunks)
	if string(result) != string(content) {
		t.Errorf("Content mismatch")
	}
}

func TestReassembleTranscript_EmptyChunks(t *testing.T) {
	result, err := ReassembleTranscript([][]byte{}, "")
	if err != nil {
		t.Fatalf("ReassembleTranscript error: %v", err)
	}
	if result != nil {
		t.Errorf("Expected nil for empty chunks, got %v", result)
	}
}

func TestReassembleJSONL_MultipleChunks(t *testing.T) {
	chunk1 := []byte(`{"line":1}`)
	chunk2 := []byte(`{"line":2}`)
	chunks := [][]byte{chunk1, chunk2}

	result := ReassembleJSONL(chunks)
	expected := []byte(`{"line":1}
{"line":2}`)
	if string(result) != string(expected) {
		t.Errorf("Content mismatch:\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestChunkJSONL_OversizedLine(t *testing.T) {
	// A single line that exceeds maxSize should return an error
	maxSize := 100
	oversizedLine := `{"type":"human","message":"` + strings.Repeat("x", maxSize) + `"}`
	content := []byte(oversizedLine)

	_, err := ChunkJSONL(content, maxSize)
	if err == nil {
		t.Fatal("Expected error for oversized line, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum chunk size") {
		t.Errorf("Expected error about exceeding chunk size, got: %v", err)
	}
}

func TestChunkJSONL_OversizedLineInMiddle(t *testing.T) {
	// An oversized line in the middle of content should return an error
	maxSize := 100
	normalLine := `{"type":"human","message":"short"}`
	oversizedLine := `{"type":"assistant","message":"` + strings.Repeat("x", maxSize) + `"}`
	content := []byte(normalLine + "\n" + oversizedLine + "\n" + normalLine)

	_, err := ChunkJSONL(content, maxSize)
	if err == nil {
		t.Fatal("Expected error for oversized line, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("Expected error to mention line 2, got: %v", err)
	}
}

func TestDetectAgentTypeFromContent(t *testing.T) {
	tests := []struct {
		name     string
		content  []byte
		expected string
	}{
		{
			name:     "Gemini JSON",
			content:  []byte(`{"messages":[{"type":"user","content":"hi"}]}`),
			expected: "Gemini CLI",
		},
		{
			name:     "JSONL",
			content:  []byte(`{"type":"human","message":"hi"}`),
			expected: "",
		},
		{
			name:     "Empty messages array",
			content:  []byte(`{"messages":[]}`),
			expected: "", // Empty messages should not be detected as Gemini
		},
		{
			name:     "Invalid JSON",
			content:  []byte(`not json`),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectAgentTypeFromContent(tt.content)
			if result != tt.expected {
				t.Errorf("DetectAgentTypeFromContent() = %q, want %q", result, tt.expected)
			}
		})
	}
}
