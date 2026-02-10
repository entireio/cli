package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGetStorageDir(t *testing.T) {
	t.Parallel()

	dir, err := GetStorageDir()
	if err != nil {
		t.Fatalf("GetStorageDir failed: %v", err)
	}

	// Should end with opencode/storage
	if !contains(dir, "opencode") || !contains(dir, "storage") {
		t.Errorf("Unexpected storage dir: %s", dir)
	}
}

func TestReconstructTranscript_SessionNotFound(t *testing.T) {
	t.Parallel()

	_, err := ReconstructTranscript("nonexistent-session-id")
	if err == nil {
		t.Error("Expected error for nonexistent session")
	}
}

func TestReconstructTranscript_RealSession(t *testing.T) {
	t.Parallel()

	// Check if we have real OpenCode storage available
	storageDir, err := GetStorageDir()
	if err != nil {
		t.Skip("Could not get storage dir")
	}

	msgDir := filepath.Join(storageDir, "message")
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		t.Skipf("Could not read message dir: %v", err)
	}

	if len(entries) == 0 {
		t.Skip("No OpenCode sessions available for testing")
	}

	// Use the first available session
	sessionID := entries[0].Name()

	data, err := ReconstructTranscript(sessionID)
	if err != nil {
		t.Fatalf("ReconstructTranscript failed: %v", err)
	}

	if len(data) == 0 {
		t.Error("Expected non-empty transcript data")
	}

	// Parse the transcript
	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript failed: %v", err)
	}

	if len(lines) == 0 {
		t.Error("Expected non-empty transcript lines")
	}

	// Verify line structure
	for i, line := range lines {
		if line.Type != "user" && line.Type != "assistant" {
			t.Errorf("Line %d has unexpected type: %s", i, line.Type)
		}
		if line.UUID == "" {
			t.Errorf("Line %d has empty UUID", i)
		}
		if len(line.Message) == 0 {
			t.Errorf("Line %d has empty message", i)
		}
	}

	t.Logf("Reconstructed %d lines from session %s", len(lines), sessionID)
}

func TestExtractLastUserPrompt(t *testing.T) {
	t.Parallel()

	// Create test transcript lines
	userMsg, _ := json.Marshal(map[string]interface{}{
		"content": "Test prompt message",
	})

	lines := []TranscriptLine{
		{Type: "user", UUID: "1", Message: userMsg},
		{Type: "assistant", UUID: "2", Message: []byte(`{"content":[]}`)},
	}

	prompt := ExtractLastUserPrompt(lines)
	if prompt != "Test prompt message" {
		t.Errorf("Expected 'Test prompt message', got '%s'", prompt)
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	// Create test transcript with a write tool call
	assistantMsg, _ := json.Marshal(map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type":  "tool_use",
				"name":  "write",
				"input": map[string]string{"file_path": "/test/file.txt"},
			},
		},
	})

	lines := []TranscriptLine{
		{Type: "assistant", UUID: "1", Message: assistantMsg},
	}

	files := ExtractModifiedFiles(lines)
	if len(files) != 1 || files[0] != "/test/file.txt" {
		t.Errorf("Expected [/test/file.txt], got %v", files)
	}
}

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()

	// Create test transcript with usage info
	assistantMsg, _ := json.Marshal(map[string]interface{}{
		"id":      "msg_123",
		"content": []interface{}{},
		"usage": map[string]interface{}{
			"input_tokens":  100,
			"output_tokens": 50,
		},
	})

	lines := []TranscriptLine{
		{Type: "assistant", UUID: "1", Message: assistantMsg},
	}

	usage := CalculateTokenUsage(lines)
	if usage.InputTokens != 100 {
		t.Errorf("Expected 100 input tokens, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("Expected 50 output tokens, got %d", usage.OutputTokens)
	}
	if usage.APICallCount != 1 {
		t.Errorf("Expected 1 API call, got %d", usage.APICallCount)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
