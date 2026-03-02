package factoryaidroid

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// TestDetectPresence uses t.Chdir so it cannot be parallel.
func TestDetectPresence(t *testing.T) {
	t.Run("factory directory exists", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		if err := os.Mkdir(".factory", 0o755); err != nil {
			t.Fatalf("failed to create .factory: %v", err)
		}

		ag := &FactoryAIDroidAgent{}
		present, err := ag.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})

	t.Run("no factory directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &FactoryAIDroidAgent{}
		present, err := ag.DetectPresence(context.Background())
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if present {
			t.Error("DetectPresence() = true, want false")
		}
	})
}

// --- Transcript tests ---

func TestReadTranscript(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "transcript.jsonl")
	content := `{"role":"user","content":"hello"}
{"role":"assistant","content":"hi"}`
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	ag := &FactoryAIDroidAgent{}
	data, err := ag.ReadTranscript(file)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if string(data) != content {
		t.Errorf("ReadTranscript() = %q, want %q", string(data), content)
	}
}

func TestReadTranscript_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}
	_, err := ag.ReadTranscript("/nonexistent/path/transcript.jsonl")
	if err == nil {
		t.Error("ReadTranscript() should error on missing file")
	}
}

func TestChunkTranscript_LargeContent(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}

	// Build multi-line JSONL that exceeds a small maxSize
	var lines []string
	for i := range 50 {
		lines = append(lines, fmt.Sprintf(`{"role":"user","content":"message %d %s"}`, i, strings.Repeat("x", 200)))
	}
	content := []byte(strings.Join(lines, "\n"))

	maxSize := 2000
	chunks, err := ag.ChunkTranscript(context.Background(), content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) < 2 {
		t.Errorf("Expected at least 2 chunks for large content, got %d", len(chunks))
	}

	// Verify each chunk is valid JSONL (each line is valid JSON)
	for i, chunk := range chunks {
		chunkLines := strings.Split(string(chunk), "\n")
		for j, line := range chunkLines {
			if line == "" {
				continue
			}
			if line[0] != '{' {
				t.Errorf("Chunk %d, line %d doesn't look like JSON: %q", i, j, line[:min(len(line), 40)])
			}
		}
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}

	original := `{"role":"user","content":"hello"}
{"role":"assistant","content":"hi there"}
{"role":"user","content":"thanks"}`

	chunks, err := ag.ChunkTranscript(context.Background(), []byte(original), 60)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	if string(reassembled) != original {
		t.Errorf("Round-trip mismatch:\n got: %q\nwant: %q", string(reassembled), original)
	}
}

func TestGetSessionDir(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}

	dir, err := ag.GetSessionDir("/Users/alisha/Projects/test-repos/factoryai-droid")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get home dir: %v", err)
	}

	expected := filepath.Join(homeDir, ".factory", "sessions", "-Users-alisha-Projects-test-repos-factoryai-droid")
	if dir != expected {
		t.Errorf("GetSessionDir() = %q, want %q", dir, expected)
	}
}

// --- ReadSession / WriteSession tests ---

func TestReadSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")

	// Write a Droid-format JSONL transcript with a file-modifying tool call
	content := `{"type":"message","id":"msg1","message":{"role":"user","content":[{"type":"text","text":"create a file"}]}}
{"type":"message","id":"msg2","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"hello.txt","content":"hi"}}]}}`
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &FactoryAIDroidAgent{}
	session, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "test-session-123",
		SessionRef: transcriptPath,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if session.SessionID != "test-session-123" {
		t.Errorf("SessionID = %q, want %q", session.SessionID, "test-session-123")
	}
	if session.AgentName != agent.AgentNameFactoryAIDroid {
		t.Errorf("AgentName = %q, want %q", session.AgentName, agent.AgentNameFactoryAIDroid)
	}
	if session.SessionRef != transcriptPath {
		t.Errorf("SessionRef = %q, want %q", session.SessionRef, transcriptPath)
	}
	if len(session.NativeData) == 0 {
		t.Error("NativeData should not be empty")
	}
	if len(session.ModifiedFiles) == 0 {
		t.Error("ModifiedFiles should contain at least one file")
	}
}

func TestReadSession_EmptyRef(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}
	_, err := ag.ReadSession(&agent.HookInput{SessionID: "test"})
	if err == nil {
		t.Error("ReadSession() should error on empty SessionRef")
	}
}

func TestReadSession_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}
	_, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "test",
		SessionRef: "/nonexistent/path/transcript.jsonl",
	})
	if err == nil {
		t.Error("ReadSession() should error on missing file")
	}
}

func TestWriteSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Write to a nested path to test directory creation
	transcriptPath := filepath.Join(tmpDir, "sessions", "project", "transcript.jsonl")
	nativeData := []byte(`{"type":"message","id":"msg1","message":{"role":"user","content":"hello"}}`)

	ag := &FactoryAIDroidAgent{}
	err := ag.WriteSession(context.Background(), &agent.AgentSession{
		SessionID:  "test-session-456",
		AgentName:  agent.AgentNameFactoryAIDroid,
		SessionRef: transcriptPath,
		NativeData: nativeData,
	})
	if err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	// Verify file was written correctly
	written, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(written) != string(nativeData) {
		t.Errorf("written data = %q, want %q", string(written), string(nativeData))
	}
}

func TestWriteSession_NilSession(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}
	if err := ag.WriteSession(context.Background(), nil); err == nil {
		t.Error("WriteSession(nil) should error")
	}
}

func TestWriteSession_WrongAgent(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}
	err := ag.WriteSession(context.Background(), &agent.AgentSession{
		AgentName:  "claude-code",
		SessionRef: "/tmp/test.jsonl",
		NativeData: []byte("data"),
	})
	if err == nil {
		t.Error("WriteSession() should error for wrong agent name")
	}
}

func TestWriteSession_EmptyRef(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}
	err := ag.WriteSession(context.Background(), &agent.AgentSession{
		AgentName:  agent.AgentNameFactoryAIDroid,
		NativeData: []byte("data"),
	})
	if err == nil {
		t.Error("WriteSession() should error on empty SessionRef")
	}
}

func TestWriteSession_EmptyNativeData(t *testing.T) {
	t.Parallel()
	ag := &FactoryAIDroidAgent{}
	err := ag.WriteSession(context.Background(), &agent.AgentSession{
		AgentName:  agent.AgentNameFactoryAIDroid,
		SessionRef: "/tmp/test.jsonl",
	})
	if err == nil {
		t.Error("WriteSession() should error on empty NativeData")
	}
}

func TestReadWriteSession_RoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	originalPath := filepath.Join(tmpDir, "original.jsonl")
	restoredPath := filepath.Join(tmpDir, "restored.jsonl")

	content := `{"type":"message","id":"msg1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"message","id":"msg2","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]}}`
	if err := os.WriteFile(originalPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write original: %v", err)
	}

	ag := &FactoryAIDroidAgent{}

	// Read from original location
	session, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "round-trip-test",
		SessionRef: originalPath,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	// Write to new location
	session.SessionRef = restoredPath
	if err := ag.WriteSession(context.Background(), session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	// Verify content matches
	restored, err := os.ReadFile(restoredPath)
	if err != nil {
		t.Fatalf("failed to read restored: %v", err)
	}
	if string(restored) != content {
		t.Errorf("round-trip mismatch:\n got: %q\nwant: %q", string(restored), content)
	}
}

// TestGetSessionDir_EnvOverride cannot use t.Parallel() due to t.Setenv.
func TestGetSessionDir_EnvOverride(t *testing.T) {
	ag := &FactoryAIDroidAgent{}
	override := "/tmp/test-droid-sessions"
	t.Setenv("ENTIRE_TEST_DROID_PROJECT_DIR", override)

	dir, err := ag.GetSessionDir("/any/repo/path")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if dir != override {
		t.Errorf("GetSessionDir() = %q, want %q (env override)", dir, override)
	}
}
