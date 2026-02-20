package geminicli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestNewGeminiCLIAgent(t *testing.T) {
	ag := NewGeminiCLIAgent()
	if ag == nil {
		t.Fatal("NewGeminiCLIAgent() returned nil")
	}

	gemini, ok := ag.(*GeminiCLIAgent)
	if !ok {
		t.Fatal("NewGeminiCLIAgent() didn't return *GeminiCLIAgent")
	}
	if gemini == nil {
		t.Fatal("NewGeminiCLIAgent() returned nil agent")
	}
}

func TestName(t *testing.T) {
	ag := &GeminiCLIAgent{}
	if name := ag.Name(); name != agent.AgentNameGemini {
		t.Errorf("Name() = %q, want %q", name, agent.AgentNameGemini)
	}
}

func TestDescription(t *testing.T) {
	ag := &GeminiCLIAgent{}
	desc := ag.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestDetectPresence(t *testing.T) {
	t.Run("no .gemini directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &GeminiCLIAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if present {
			t.Error("DetectPresence() = true, want false")
		}
	})

	t.Run("with .gemini directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		// Create .gemini directory
		if err := os.Mkdir(".gemini", 0o755); err != nil {
			t.Fatalf("failed to create .gemini: %v", err)
		}

		ag := &GeminiCLIAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})
}

func TestGetSessionID(t *testing.T) {
	ag := &GeminiCLIAgent{}
	input := &agent.HookInput{SessionID: "test-session-123"}

	id := ag.GetSessionID(input)
	if id != "test-session-123" {
		t.Errorf("GetSessionID() = %q, want test-session-123", id)
	}
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()

	t.Run("finds existing Gemini-named file", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		ag := &GeminiCLIAgent{}

		// Create a file with Gemini's naming convention
		geminiFile := filepath.Join(tmpDir, "session-2026-02-10T09-19-0544a0f5.json")
		if err := os.WriteFile(geminiFile, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}

		result := ag.ResolveSessionFile(tmpDir, "0544a0f5-46a6-41b3-a89c-e7804df731b8")
		if result != geminiFile {
			t.Errorf("ResolveSessionFile() = %q, want %q", result, geminiFile)
		}
	})

	t.Run("falls back to Gemini-style filename when no match", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		ag := &GeminiCLIAgent{}

		result := ag.ResolveSessionFile(tmpDir, "0544a0f5-46a6-41b3-a89c-e7804df731b8")
		filename := filepath.Base(result)
		if !strings.HasPrefix(filename, "session-") {
			t.Errorf("fallback filename %q should start with 'session-'", filename)
		}
		if !strings.HasSuffix(filename, "-0544a0f5.json") {
			t.Errorf("fallback filename %q should end with '-0544a0f5.json'", filename)
		}
		if filepath.Dir(result) != tmpDir {
			t.Errorf("fallback dir = %q, want %q", filepath.Dir(result), tmpDir)
		}
	})

	t.Run("handles short session ID", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		ag := &GeminiCLIAgent{}

		// Short ID (less than 8 chars) should use entire ID in filename
		result := ag.ResolveSessionFile(tmpDir, "abc123")
		filename := filepath.Base(result)
		if !strings.HasPrefix(filename, "session-") {
			t.Errorf("fallback filename %q should start with 'session-'", filename)
		}
		if !strings.HasSuffix(filename, "-abc123.json") {
			t.Errorf("fallback filename %q should end with '-abc123.json'", filename)
		}
	})
}

func TestProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &GeminiCLIAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".gemini" {
		t.Errorf("ProtectedDirs() = %v, want [.gemini]", dirs)
	}
}

func TestGetSessionDir(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Test with override env var
	t.Setenv("ENTIRE_TEST_GEMINI_PROJECT_DIR", "/test/override")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if dir != "/test/override" {
		t.Errorf("GetSessionDir() = %q, want /test/override", dir)
	}
}

func TestGetSessionDir_DefaultPath(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Make sure env var is not set
	t.Setenv("ENTIRE_TEST_GEMINI_PROJECT_DIR", "")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}

	// Should contain .gemini/tmp and end with /chats
	if !filepath.IsAbs(dir) {
		t.Errorf("GetSessionDir() should return absolute path, got %q", dir)
	}
}

func TestFormatResumeCommand(t *testing.T) {
	ag := &GeminiCLIAgent{}

	cmd := ag.FormatResumeCommand("abc123")
	expected := "gemini --resume abc123"
	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

func TestReadSession(t *testing.T) {
	tempDir := t.TempDir()

	// Create a transcript file
	transcriptPath := filepath.Join(tempDir, "transcript.json")
	transcriptContent := `{"messages": [{"role": "user", "content": "hello"}]}`
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &GeminiCLIAgent{}
	input := &agent.HookInput{
		SessionID:  "test-session",
		SessionRef: transcriptPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if session.SessionID != "test-session" {
		t.Errorf("SessionID = %q, want test-session", session.SessionID)
	}
	if session.AgentName != agent.AgentNameGemini {
		t.Errorf("AgentName = %q, want %q", session.AgentName, agent.AgentNameGemini)
	}
	if len(session.NativeData) == 0 {
		t.Error("NativeData is empty")
	}
}

func TestReadSession_NoSessionRef(t *testing.T) {
	ag := &GeminiCLIAgent{}
	input := &agent.HookInput{SessionID: "test-session"}

	_, err := ag.ReadSession(input)
	if err == nil {
		t.Error("ReadSession() should error when SessionRef is empty")
	}
}

func TestWriteSession(t *testing.T) {
	tempDir := t.TempDir()
	transcriptPath := filepath.Join(tempDir, "transcript.json")

	ag := &GeminiCLIAgent{}
	session := &agent.AgentSession{
		SessionID:  "test-session",
		AgentName:  agent.AgentNameGemini,
		SessionRef: transcriptPath,
		NativeData: []byte(`{"messages": []}`),
	}

	err := ag.WriteSession(session)
	if err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	// Verify file was written
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read transcript: %v", err)
	}

	if string(data) != `{"messages": []}` {
		t.Errorf("transcript content = %q, want {\"messages\": []}", string(data))
	}
}

func TestWriteSession_Nil(t *testing.T) {
	ag := &GeminiCLIAgent{}

	err := ag.WriteSession(nil)
	if err == nil {
		t.Error("WriteSession(nil) should error")
	}
}

func TestWriteSession_WrongAgent(t *testing.T) {
	ag := &GeminiCLIAgent{}
	session := &agent.AgentSession{
		AgentName:  "claude-code",
		SessionRef: "/path/to/file",
		NativeData: []byte("{}"),
	}

	err := ag.WriteSession(session)
	if err == nil {
		t.Error("WriteSession() should error for wrong agent")
	}
}

func TestWriteSession_NoSessionRef(t *testing.T) {
	ag := &GeminiCLIAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameGemini,
		NativeData: []byte("{}"),
	}

	err := ag.WriteSession(session)
	if err == nil {
		t.Error("WriteSession() should error when SessionRef is empty")
	}
}

func TestWriteSession_NoNativeData(t *testing.T) {
	ag := &GeminiCLIAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameGemini,
		SessionRef: "/path/to/file",
	}

	err := ag.WriteSession(session)
	if err == nil {
		t.Error("WriteSession() should error when NativeData is empty")
	}
}

func TestGetProjectHash(t *testing.T) {
	t.Parallel()

	// GetProjectHash should return a consistent SHA256 hex string for a given path
	hash1 := GetProjectHash("/Users/test/project")
	hash2 := GetProjectHash("/Users/test/project")
	if hash1 != hash2 {
		t.Errorf("GetProjectHash should be deterministic: got %q and %q", hash1, hash2)
	}

	// Should be a 64-char hex string (SHA256)
	if len(hash1) != 64 {
		t.Errorf("GetProjectHash should return 64-char hex string, got %d chars: %q", len(hash1), hash1)
	}

	// Different paths should produce different hashes
	hash3 := GetProjectHash("/Users/test/other")
	if hash1 == hash3 {
		t.Errorf("GetProjectHash should return different hashes for different paths")
	}
}

// Chunking tests

func TestChunkTranscript_SmallContent(t *testing.T) {
	ag := &GeminiCLIAgent{}

	content := []byte(`{"messages":[{"type":"user","content":"hello"},{"type":"gemini","content":"hi there"}]}`)

	chunks, err := ag.ChunkTranscript(content, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk, got %d", len(chunks))
	}
}

func TestChunkTranscript_LargeContent(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Create a transcript with many messages that exceeds maxSize
	var messages []GeminiMessage
	for i := range 100 {
		messages = append(messages, GeminiMessage{
			Type:    "user",
			Content: fmt.Sprintf("message %d with some content to make it larger: %s", i, strings.Repeat("x", 500)),
		})
	}

	transcript := GeminiTranscript{Messages: messages}
	content, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("Failed to marshal test transcript: %v", err)
	}

	// Use a small maxSize to force chunking
	maxSize := 5000
	chunks, err := ag.ChunkTranscript(content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	if len(chunks) < 2 {
		t.Errorf("Expected at least 2 chunks for large content, got %d", len(chunks))
	}

	// Verify each chunk is valid JSON with messages array
	for i, chunk := range chunks {
		var parsed GeminiTranscript
		if err := json.Unmarshal(chunk, &parsed); err != nil {
			t.Errorf("Chunk %d is not valid Gemini JSON: %v", i, err)
		}
		if len(parsed.Messages) == 0 {
			t.Errorf("Chunk %d has no messages", i)
		}
	}

	// Verify reassembly gives back all messages
	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	var result GeminiTranscript
	if err := json.Unmarshal(reassembled, &result); err != nil {
		t.Fatalf("Failed to unmarshal reassembled content: %v", err)
	}

	if len(result.Messages) != len(messages) {
		t.Errorf("Reassembled message count = %d, want %d", len(result.Messages), len(messages))
	}
}

func TestChunkTranscript_EmptyMessages(t *testing.T) {
	ag := &GeminiCLIAgent{}

	content := []byte(`{"messages":[]}`)

	chunks, err := ag.ChunkTranscript(content, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk for empty messages, got %d", len(chunks))
	}
	if string(chunks[0]) != string(content) {
		t.Errorf("Expected original content preserved, got %s", chunks[0])
	}
}

func TestChunkTranscript_InvalidJSON_FallsBackToJSONL(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Invalid JSON that looks like JSONL
	content := []byte(`{"type":"user","content":"hello"}
{"type":"gemini","content":"hi"}`)

	chunks, err := ag.ChunkTranscript(content, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	// Should fall back to JSONL chunking and return 1 chunk for small content
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk (JSONL fallback), got %d", len(chunks))
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Create a realistic transcript
	original := GeminiTranscript{
		Messages: []GeminiMessage{
			{Type: "user", Content: "Write a hello world program"},
			{Type: "gemini", Content: "Sure, here's a hello world program:", ToolCalls: []GeminiToolCall{
				{ID: "1", Name: "write_file", Args: map[string]interface{}{"path": "main.go", "content": "package main\n\nfunc main() {\n\tprintln(\"Hello, World!\")\n}"}},
			}},
			{Type: "user", Content: "Now add a function"},
			{Type: "gemini", Content: "I'll add a greet function:", ToolCalls: []GeminiToolCall{
				{ID: "2", Name: "edit_file", Args: map[string]interface{}{"path": "main.go"}},
			}},
		},
	}

	content, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal original: %v", err)
	}

	// Use small maxSize to force chunking
	maxSize := 200
	chunks, err := ag.ChunkTranscript(content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	var result GeminiTranscript
	if err := json.Unmarshal(reassembled, &result); err != nil {
		t.Fatalf("Failed to unmarshal reassembled: %v", err)
	}

	if len(result.Messages) != len(original.Messages) {
		t.Fatalf("Message count mismatch: got %d, want %d", len(result.Messages), len(original.Messages))
	}

	for i, msg := range result.Messages {
		if msg.Type != original.Messages[i].Type {
			t.Errorf("Message %d type = %q, want %q", i, msg.Type, original.Messages[i].Type)
		}
		if msg.Content != original.Messages[i].Content {
			t.Errorf("Message %d content mismatch", i)
		}
		if len(msg.ToolCalls) != len(original.Messages[i].ToolCalls) {
			t.Errorf("Message %d toolCalls count = %d, want %d", i, len(msg.ToolCalls), len(original.Messages[i].ToolCalls))
		}
	}
}

func TestReassembleTranscript_SingleChunk(t *testing.T) {
	ag := &GeminiCLIAgent{}

	content := []byte(`{"messages":[{"type":"user","content":"hello"}]}`)
	chunks := [][]byte{content}

	result, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	var parsed GeminiTranscript
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if len(parsed.Messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(parsed.Messages))
	}
}

func TestReassembleTranscript_MultipleChunks(t *testing.T) {
	ag := &GeminiCLIAgent{}

	chunk1 := []byte(`{"messages":[{"type":"user","content":"hello"}]}`)
	chunk2 := []byte(`{"messages":[{"type":"gemini","content":"hi"}]}`)
	chunks := [][]byte{chunk1, chunk2}

	result, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	var parsed GeminiTranscript
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if len(parsed.Messages) != 2 {
		t.Errorf("Expected 2 messages, got %d", len(parsed.Messages))
	}
	if parsed.Messages[0].Content != "hello" {
		t.Errorf("First message content = %q, want hello", parsed.Messages[0].Content)
	}
	if parsed.Messages[1].Content != "hi" {
		t.Errorf("Second message content = %q, want hi", parsed.Messages[1].Content)
	}
}

func TestReassembleTranscript_InvalidChunk(t *testing.T) {
	ag := &GeminiCLIAgent{}

	chunk1 := []byte(`{"messages":[{"type":"user","content":"hello"}]}`)
	chunk2 := []byte(`not valid json`)
	chunks := [][]byte{chunk1, chunk2}

	_, err := ag.ReassembleTranscript(chunks)
	if err == nil {
		t.Error("ReassembleTranscript() should error on invalid JSON chunk")
	}
}

func TestReassembleTranscript_EmptyChunks(t *testing.T) {
	ag := &GeminiCLIAgent{}

	result, err := ag.ReassembleTranscript([][]byte{})
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	// Should return valid JSON with empty messages array
	var parsed GeminiTranscript
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if len(parsed.Messages) != 0 {
		t.Errorf("Expected 0 messages for empty chunks, got %d", len(parsed.Messages))
	}
}

func TestChunkTranscript_SingleOversizedMessage(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Create a single message that exceeds maxSize
	largeContent := strings.Repeat("x", 1000)
	transcript := GeminiTranscript{
		Messages: []GeminiMessage{
			{Type: "user", Content: largeContent},
		},
	}

	content, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	// maxSize smaller than the single message
	maxSize := 100
	chunks, err := ag.ChunkTranscript(content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	// Should still produce a chunk (can't split a single message)
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk for single oversized message, got %d", len(chunks))
	}

	// Verify it's valid and contains the message
	var parsed GeminiTranscript
	if err := json.Unmarshal(chunks[0], &parsed); err != nil {
		t.Fatalf("Chunk is not valid JSON: %v", err)
	}
	if len(parsed.Messages) != 1 {
		t.Errorf("Expected 1 message in chunk, got %d", len(parsed.Messages))
	}
}

func TestChunkTranscript_ChunkBoundary(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Create messages where the boundary matters
	messages := []GeminiMessage{
		{Type: "user", Content: "msg1"},
		{Type: "gemini", Content: "msg2"},
		{Type: "user", Content: "msg3"},
		{Type: "gemini", Content: "msg4"},
	}

	transcript := GeminiTranscript{Messages: messages}
	content, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	// Calculate size to get exactly 2 chunks with 2 messages each
	// The base structure is {"messages":[]} = 15 chars
	// Each message is roughly 25-30 chars including comma
	maxSize := 100

	chunks, err := ag.ChunkTranscript(content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	// Verify all messages are preserved across chunks
	totalMessages := 0
	for _, chunk := range chunks {
		var parsed GeminiTranscript
		if err := json.Unmarshal(chunk, &parsed); err != nil {
			t.Fatalf("Chunk is not valid JSON: %v", err)
		}
		totalMessages += len(parsed.Messages)
	}

	if totalMessages != len(messages) {
		t.Errorf("Total messages across chunks = %d, want %d", totalMessages, len(messages))
	}
}

func TestChunkTranscript_PreservesMessageOrder(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// Create messages with numbered content to verify order
	var messages []GeminiMessage
	for i := range 20 {
		messages = append(messages, GeminiMessage{
			Type:    "user",
			Content: fmt.Sprintf("message-%03d", i),
		})
	}

	transcript := GeminiTranscript{Messages: messages}
	content, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	// Small maxSize to force multiple chunks
	chunks, err := ag.ChunkTranscript(content, 200)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	var result GeminiTranscript
	if err := json.Unmarshal(reassembled, &result); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	// Verify message order is preserved
	for i, msg := range result.Messages {
		expected := fmt.Sprintf("message-%03d", i)
		if msg.Content != expected {
			t.Errorf("Message %d content = %q, want %q", i, msg.Content, expected)
		}
	}
}
