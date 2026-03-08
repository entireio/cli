package copilotcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// sampleTranscriptLines returns JSONL lines matching real Copilot CLI transcript format.
func sampleTranscriptLines() []string {
	return []string{
		`{"type":"session.start","data":{"sessionId":"test-sess","version":1},"id":"c1","timestamp":"2026-03-02T01:00:00Z","parentId":null}`,
		`{"type":"user.message","data":{"content":"hello","interactionId":"i1"},"id":"u1","timestamp":"2026-03-02T01:00:01Z","parentId":"c1"}`,
		`{"type":"assistant.turn_start","data":{"turnId":"0","interactionId":"i1"},"id":"t1","timestamp":"2026-03-02T01:00:02Z","parentId":"u1"}`,
		`{"type":"assistant.message","data":{"content":"Hi there!","toolRequests":[],"interactionId":"i1"},"id":"m1","timestamp":"2026-03-02T01:00:03Z","parentId":"t1"}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"te1","timestamp":"2026-03-02T01:00:04Z","parentId":"m1"}`,
	}
}

func writeSampleTranscript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	content := strings.Join(sampleTranscriptLines(), "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write sample transcript: %v", err)
	}
	return path
}

// --- Identity ---

func TestCopilotCLIAgent_Name(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	if ag.Name() != agent.AgentNameCopilotCLI {
		t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameCopilotCLI)
	}
}

func TestCopilotCLIAgent_Type(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	if ag.Type() != agent.AgentTypeCopilotCLI {
		t.Errorf("Type() = %q, want %q", ag.Type(), agent.AgentTypeCopilotCLI)
	}
}

func TestCopilotCLIAgent_Description(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	if ag.Description() == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCopilotCLIAgent_IsPreview(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	if !ag.IsPreview() {
		t.Error("IsPreview() = false, want true")
	}
}

func TestCopilotCLIAgent_ProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	dirs := ag.ProtectedDirs()
	expected := filepath.Join(".github", "hooks")
	if len(dirs) != 1 || dirs[0] != expected {
		t.Errorf("ProtectedDirs() = %v, want [%s]", dirs, expected)
	}
}

func TestCopilotCLIAgent_FormatResumeCommand(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	cmd := ag.FormatResumeCommand("b0ff98c0-8e01-4b73-bf92-9649b139931b")
	expected := "copilot --resume b0ff98c0-8e01-4b73-bf92-9649b139931b"
	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

// --- GetSessionID ---

func TestCopilotCLIAgent_GetSessionID(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	input := &agent.HookInput{SessionID: "copilot-sess-42"}
	if id := ag.GetSessionID(input); id != "copilot-sess-42" {
		t.Errorf("GetSessionID() = %q, want copilot-sess-42", id)
	}
}

// --- ResolveSessionFile ---

func TestCopilotCLIAgent_ResolveSessionFile(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	result := ag.ResolveSessionFile("/home/user/.copilot/session-state", "b0ff98c0-8e01-4b73-bf92-9649b139931b")
	expected := "/home/user/.copilot/session-state/b0ff98c0-8e01-4b73-bf92-9649b139931b/events.jsonl"
	if result != expected {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, expected)
	}
}

// --- GetSessionDir ---

func TestCopilotCLIAgent_GetSessionDir_EnvOverride(t *testing.T) {
	ag := &CopilotCLIAgent{}
	t.Setenv("ENTIRE_TEST_COPILOT_SESSION_DIR", "/test/override")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if dir != "/test/override" {
		t.Errorf("GetSessionDir() = %q, want /test/override", dir)
	}
}

func TestCopilotCLIAgent_GetSessionDir_DefaultPath(t *testing.T) {
	ag := &CopilotCLIAgent{}
	t.Setenv("ENTIRE_TEST_COPILOT_SESSION_DIR", "")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("GetSessionDir() should return absolute path, got %q", dir)
	}
	if !strings.Contains(dir, ".copilot") {
		t.Errorf("GetSessionDir() = %q, expected path containing .copilot", dir)
	}
	if !strings.HasSuffix(dir, "session-state") {
		t.Errorf("GetSessionDir() = %q, expected path ending with session-state", dir)
	}
}

// --- ReadSession ---

func TestReadSession_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CopilotCLIAgent{}
	input := &agent.HookInput{
		SessionID:  "copilot-session-1",
		SessionRef: transcriptPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if session.SessionID != "copilot-session-1" {
		t.Errorf("SessionID = %q, want copilot-session-1", session.SessionID)
	}
	if session.AgentName != agent.AgentNameCopilotCLI {
		t.Errorf("AgentName = %q, want %q", session.AgentName, agent.AgentNameCopilotCLI)
	}
	if session.SessionRef != transcriptPath {
		t.Errorf("SessionRef = %q, want %q", session.SessionRef, transcriptPath)
	}
	if len(session.NativeData) == 0 {
		t.Error("NativeData is empty")
	}
	if session.StartTime.IsZero() {
		t.Error("StartTime is zero")
	}
}

func TestReadSession_NativeDataMatchesFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CopilotCLIAgent{}
	input := &agent.HookInput{
		SessionID:  "sess-read",
		SessionRef: transcriptPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	fileData, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read transcript file: %v", err)
	}

	if !bytes.Equal(session.NativeData, fileData) {
		t.Error("NativeData does not match file contents")
	}
}

func TestReadSession_EmptySessionRef(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	input := &agent.HookInput{SessionID: "sess-no-ref"}

	_, err := ag.ReadSession(input)
	if err == nil {
		t.Fatal("ReadSession() should error when SessionRef is empty")
	}
}

func TestReadSession_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	input := &agent.HookInput{
		SessionID:  "sess-missing",
		SessionRef: "/nonexistent/path/events.jsonl",
	}

	_, err := ag.ReadSession(input)
	if err == nil {
		t.Fatal("ReadSession() should error when transcript file doesn't exist")
	}
}

// --- WriteSession ---

func TestWriteSession_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "output.jsonl")

	content := strings.Join(sampleTranscriptLines(), "\n") + "\n"

	ag := &CopilotCLIAgent{}
	session := &agent.AgentSession{
		SessionID:  "write-session-1",
		AgentName:  agent.AgentNameCopilotCLI,
		SessionRef: transcriptPath,
		NativeData: []byte(content),
	}

	if err := ag.WriteSession(context.Background(), session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(data) != content {
		t.Errorf("written content does not match original")
	}
}

func TestWriteSession_RoundTrip(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CopilotCLIAgent{}

	// Read
	input := &agent.HookInput{
		SessionID:  "roundtrip-session",
		SessionRef: transcriptPath,
	}
	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	// Write to new path
	newPath := filepath.Join(tmpDir, "roundtrip.jsonl")
	session.SessionRef = newPath
	if err := ag.WriteSession(context.Background(), session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	// Read back and compare
	original, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read original: %v", err)
	}
	written, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("failed to read written: %v", err)
	}
	if !bytes.Equal(original, written) {
		t.Error("round-trip data mismatch: written file differs from original")
	}
}

func TestWriteSession_Nil(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	if err := ag.WriteSession(context.Background(), nil); err == nil {
		t.Error("WriteSession(nil) should error")
	}
}

func TestWriteSession_WrongAgent(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	session := &agent.AgentSession{
		AgentName:  "claude-code",
		SessionRef: "/path/to/file",
		NativeData: []byte("data"),
	}
	if err := ag.WriteSession(context.Background(), session); err == nil {
		t.Error("WriteSession() should error for wrong agent")
	}
}

func TestWriteSession_EmptyAgentName(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "empty-agent.jsonl")

	ag := &CopilotCLIAgent{}
	session := &agent.AgentSession{
		AgentName:  "", // Empty agent name should be accepted
		SessionRef: transcriptPath,
		NativeData: []byte("data"),
	}
	if err := ag.WriteSession(context.Background(), session); err != nil {
		t.Errorf("WriteSession() with empty AgentName should succeed, got: %v", err)
	}
}

func TestWriteSession_NoSessionRef(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameCopilotCLI,
		NativeData: []byte("data"),
	}
	if err := ag.WriteSession(context.Background(), session); err == nil {
		t.Error("WriteSession() should error when SessionRef is empty")
	}
}

func TestWriteSession_NoNativeData(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameCopilotCLI,
		SessionRef: "/path/to/file",
	}
	if err := ag.WriteSession(context.Background(), session); err == nil {
		t.Error("WriteSession() should error when NativeData is empty")
	}
}

// --- ChunkTranscript / ReassembleTranscript ---

func TestChunkTranscript_SmallContent(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	content := []byte(strings.Join(sampleTranscriptLines(), "\n"))

	chunks, err := ag.ChunkTranscript(context.Background(), content, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for small content, got %d", len(chunks))
	}
	if !bytes.Equal(chunks[0], content) {
		t.Error("single chunk should be identical to input")
	}
}

func TestChunkTranscript_ForcesMultipleChunks(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	var lines []string
	for range 20 {
		lines = append(lines, `{"type":"user.message","data":{"content":"`+strings.Repeat("x", 100)+`"},"id":"u","timestamp":"2026-01-01T00:00:00Z","parentId":null}`)
	}
	content := []byte(strings.Join(lines, "\n"))

	maxSize := 500
	chunks, err := ag.ChunkTranscript(context.Background(), content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks, got %d", len(chunks))
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	original := []byte(strings.Join(sampleTranscriptLines(), "\n"))

	chunks, err := ag.ChunkTranscript(context.Background(), original, 300)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	if !bytes.Equal(original, reassembled) {
		t.Errorf("round-trip mismatch:\n  original len=%d\n  reassembled len=%d", len(original), len(reassembled))
	}
}

func TestChunkTranscript_EmptyContent(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	chunks, err := ag.ChunkTranscript(context.Background(), []byte{}, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty content, got %d", len(chunks))
	}
}

func TestReassembleTranscript_EmptyChunks(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	result, err := ag.ReassembleTranscript([][]byte{})
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for empty chunks, got %d bytes", len(result))
	}
}

// --- DetectPresence ---

func TestDetectPresence_NoGitHubHooksDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	ag := &CopilotCLIAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if present {
		t.Error("DetectPresence() = true, want false")
	}
}

func TestDetectPresence_WithGitHubHooksDirButNoEntireJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".github", "hooks"), 0o755); err != nil {
		t.Fatalf("failed to create .github/hooks: %v", err)
	}

	initGitRepo(t, tmpDir)

	ag := &CopilotCLIAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if present {
		t.Error("DetectPresence() = true, want false (no entire.json)")
	}
}

func TestDetectPresence_WithEntireHooks(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	hooksPath := filepath.Join(tmpDir, ".github", "hooks")
	if err := os.MkdirAll(hooksPath, 0o755); err != nil {
		t.Fatalf("failed to create .github/hooks: %v", err)
	}

	entireJSON := `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {"type": "command", "bash": "entire hooks copilot-cli session-start"}
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(hooksPath, "entire.json"), []byte(entireJSON), 0o644); err != nil {
		t.Fatalf("failed to write entire.json: %v", err)
	}

	ag := &CopilotCLIAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if !present {
		t.Error("DetectPresence() = false, want true (entire.json has Entire hooks)")
	}
}

// --- ReadTranscript ---

func TestReadTranscript_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CopilotCLIAgent{}
	data, err := ag.ReadTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if len(data) == 0 {
		t.Error("ReadTranscript() returned empty data")
	}

	content := string(data)
	if !strings.Contains(content, `"type":"session.start"`) {
		t.Error("transcript missing session.start event type")
	}
	if !strings.Contains(content, `"type":"user.message"`) {
		t.Error("transcript missing user.message event type")
	}
}

func TestReadTranscript_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	_, err := ag.ReadTranscript("/nonexistent/path/events.jsonl")
	if err == nil {
		t.Fatal("ReadTranscript() should error for missing file")
	}
}

func TestReadTranscript_MatchesReadSession(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CopilotCLIAgent{}

	transcriptData, err := ag.ReadTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}

	session, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "compare-session",
		SessionRef: transcriptPath,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if !bytes.Equal(transcriptData, session.NativeData) {
		t.Error("ReadTranscript() and ReadSession().NativeData should return identical bytes")
	}
}

// --- helpers ---

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("failed to create .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("failed to write HEAD: %v", err)
	}
}
