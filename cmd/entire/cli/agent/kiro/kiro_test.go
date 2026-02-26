package kiro

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// sampleSessionContent returns sample session data for testing.
func sampleSessionContent() string {
	return `{"role":"user","content":"hello"}
{"role":"assistant","content":"Hi there! How can I help?"}
{"role":"user","content":"create a file"}
{"role":"assistant","content":"Done! Created the file."}
`
}

func writeSampleSession(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(sampleSessionContent()), 0o644); err != nil {
		t.Fatalf("failed to write sample session: %v", err)
	}
	return path
}

// --- Identity ---

func TestKiroAgent_Name(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	if ag.Name() != agent.AgentNameKiro {
		t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameKiro)
	}
}

func TestKiroAgent_Type(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	if ag.Type() != agent.AgentTypeKiro {
		t.Errorf("Type() = %q, want %q", ag.Type(), agent.AgentTypeKiro)
	}
}

func TestKiroAgent_Description(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	if ag.Description() == "" {
		t.Error("Description() returned empty string")
	}
}

func TestKiroAgent_IsPreview(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	if !ag.IsPreview() {
		t.Error("IsPreview() = false, want true")
	}
}

func TestKiroAgent_ProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".kiro" {
		t.Errorf("ProtectedDirs() = %v, want [.kiro]", dirs)
	}
}

func TestKiroAgent_FormatResumeCommand(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	cmd := ag.FormatResumeCommand("some-session-id")
	if !strings.Contains(cmd, "kiro-cli") {
		t.Errorf("FormatResumeCommand() = %q, expected mention of kiro-cli", cmd)
	}
}

// --- GetSessionID ---

func TestKiroAgent_GetSessionID(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	input := &agent.HookInput{SessionID: "kiro-sess-42"}
	if id := ag.GetSessionID(input); id != "kiro-sess-42" {
		t.Errorf("GetSessionID() = %q, want kiro-sess-42", id)
	}
}

// --- ResolveSessionFile ---

func TestKiroAgent_ResolveSessionFile(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	result := ag.ResolveSessionFile("/tmp/sessions", "abc123")
	expected := "/tmp/sessions/abc123.json"
	if result != expected {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, expected)
	}
}

// --- GetSessionDir ---

func TestKiroAgent_GetSessionDir_EnvOverride(t *testing.T) {
	ag := &KiroAgent{}
	t.Setenv("ENTIRE_TEST_KIRO_PROJECT_DIR", "/test/override")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if dir != "/test/override" {
		t.Errorf("GetSessionDir() = %q, want /test/override", dir)
	}
}

func TestKiroAgent_GetSessionDir_DefaultPath(t *testing.T) {
	ag := &KiroAgent{}
	t.Setenv("ENTIRE_TEST_KIRO_PROJECT_DIR", "")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("GetSessionDir() should return absolute path, got %q", dir)
	}
	if !strings.Contains(dir, ".kiro") {
		t.Errorf("GetSessionDir() = %q, expected path containing .kiro", dir)
	}
}

// --- ReadSession ---

func TestReadSession_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	sessionPath := writeSampleSession(t, tmpDir)

	ag := &KiroAgent{}
	input := &agent.HookInput{
		SessionID:  "kiro-session-1",
		SessionRef: sessionPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if session.SessionID != "kiro-session-1" {
		t.Errorf("SessionID = %q, want kiro-session-1", session.SessionID)
	}
	if session.AgentName != agent.AgentNameKiro {
		t.Errorf("AgentName = %q, want %q", session.AgentName, agent.AgentNameKiro)
	}
	if session.SessionRef != sessionPath {
		t.Errorf("SessionRef = %q, want %q", session.SessionRef, sessionPath)
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
	sessionPath := writeSampleSession(t, tmpDir)

	ag := &KiroAgent{}
	input := &agent.HookInput{
		SessionID:  "sess-read",
		SessionRef: sessionPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	fileData, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("failed to read session file: %v", err)
	}

	if !bytes.Equal(session.NativeData, fileData) {
		t.Error("NativeData does not match file contents")
	}
}

func TestReadSession_EmptySessionRef(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	input := &agent.HookInput{SessionID: "sess-no-ref"}

	_, err := ag.ReadSession(input)
	if err == nil {
		t.Fatal("ReadSession() should error when SessionRef is empty")
	}
}

func TestReadSession_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	input := &agent.HookInput{
		SessionID:  "sess-missing",
		SessionRef: "/nonexistent/path/session.json",
	}

	_, err := ag.ReadSession(input)
	if err == nil {
		t.Fatal("ReadSession() should error when session file doesn't exist")
	}
}

// --- WriteSession ---

func TestWriteSession_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	sessionPath := filepath.Join(tmpDir, "output.json")

	content := sampleSessionContent()

	ag := &KiroAgent{}
	session := &agent.AgentSession{
		SessionID:  "write-session-1",
		AgentName:  agent.AgentNameKiro,
		SessionRef: sessionPath,
		NativeData: []byte(content),
	}

	if err := ag.WriteSession(context.Background(), session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	data, err := os.ReadFile(sessionPath)
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
	sessionPath := writeSampleSession(t, tmpDir)

	ag := &KiroAgent{}

	// Read
	input := &agent.HookInput{
		SessionID:  "roundtrip-session",
		SessionRef: sessionPath,
	}
	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	// Write to new path
	newPath := filepath.Join(tmpDir, "roundtrip.json")
	session.SessionRef = newPath
	if err := ag.WriteSession(context.Background(), session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	// Read back and compare
	original, err := os.ReadFile(sessionPath)
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
	ag := &KiroAgent{}
	if err := ag.WriteSession(context.Background(), nil); err == nil {
		t.Error("WriteSession(nil) should error")
	}
}

func TestWriteSession_WrongAgent(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
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
	sessionPath := filepath.Join(tmpDir, "empty-agent.json")

	ag := &KiroAgent{}
	session := &agent.AgentSession{
		AgentName:  "", // Empty agent name should be accepted
		SessionRef: sessionPath,
		NativeData: []byte("data"),
	}
	if err := ag.WriteSession(context.Background(), session); err != nil {
		t.Errorf("WriteSession() with empty AgentName should succeed, got: %v", err)
	}
}

func TestWriteSession_NoSessionRef(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameKiro,
		NativeData: []byte("data"),
	}
	if err := ag.WriteSession(context.Background(), session); err == nil {
		t.Error("WriteSession() should error when SessionRef is empty")
	}
}

func TestWriteSession_NoNativeData(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameKiro,
		SessionRef: "/path/to/file",
	}
	if err := ag.WriteSession(context.Background(), session); err == nil {
		t.Error("WriteSession() should error when NativeData is empty")
	}
}

// --- ChunkTranscript / ReassembleTranscript ---

func TestChunkTranscript_SmallContent(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}

	content := []byte(sampleSessionContent())

	chunks, err := ag.ChunkTranscript(context.Background(), content, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for small content, got %d", len(chunks))
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}

	original := []byte(sampleSessionContent())

	// Use a max size large enough for individual lines but small enough to force splits
	chunks, err := ag.ChunkTranscript(context.Background(), original, 100)
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
	ag := &KiroAgent{}

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
	ag := &KiroAgent{}

	result, err := ag.ReassembleTranscript([][]byte{})
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for empty chunks, got %d bytes", len(result))
	}
}

// --- ReadTranscript ---

func TestReadTranscript_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	sessionPath := writeSampleSession(t, tmpDir)

	ag := &KiroAgent{}
	data, err := ag.ReadTranscript(sessionPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if len(data) == 0 {
		t.Error("ReadTranscript() returned empty data")
	}
}

func TestReadTranscript_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &KiroAgent{}
	_, err := ag.ReadTranscript("/nonexistent/path/session.json")
	if err == nil {
		t.Fatal("ReadTranscript() should error for missing file")
	}
}

func TestReadTranscript_MatchesReadSession(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	sessionPath := writeSampleSession(t, tmpDir)

	ag := &KiroAgent{}

	transcriptData, err := ag.ReadTranscript(sessionPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}

	session, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "compare-session",
		SessionRef: sessionPath,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if !bytes.Equal(transcriptData, session.NativeData) {
		t.Error("ReadTranscript() and ReadSession().NativeData should return identical bytes")
	}
}

// --- DetectPresence ---

func TestDetectPresence_NoKiroDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	ag := &KiroAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if present {
		t.Error("DetectPresence() = true, want false")
	}
}

func TestDetectPresence_WithKiroDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".kiro"), 0o755); err != nil {
		t.Fatalf("failed to create .kiro: %v", err)
	}

	initGitRepo(t, tmpDir)

	ag := &KiroAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if !present {
		t.Error("DetectPresence() = false, want true")
	}
}

// --- sanitizePathForKiro ---

func TestSanitizePathForKiro(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		{"/Users/robin/project", "-Users-robin-project"},
		{"/tmp/test", "-tmp-test"},
		{"simple", "simple"},
		{"/path/with spaces/dir", "-path-with-spaces-dir"},
		{"/path.with.dots/dir", "-path-with-dots-dir"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			result := sanitizePathForKiro(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizePathForKiro(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
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
