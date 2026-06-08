package cursor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// sampleTranscriptLines returns JSONL lines matching real Cursor transcript format.
// Based on an actual Cursor session: uses "role" (not "type"), wraps user text
// in <user_query> tags, and contains no tool_use blocks.
func sampleTranscriptLines() []string {
	return []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nhello\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"Hi there!"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nadd 'one' to a file and commit\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"Created one.txt with one and committed."}]}}`,
	}
}

func writeSampleTranscript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "transcript.jsonl")
	content := strings.Join(sampleTranscriptLines(), "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write sample transcript: %v", err)
	}
	return path
}

// --- Identity ---

func TestCursorAgent_Name(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	if ag.Name() != agent.AgentNameCursor {
		t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameCursor)
	}
}

func TestCursorAgent_Type(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	if ag.Type() != agent.AgentTypeCursor {
		t.Errorf("Type() = %q, want %q", ag.Type(), agent.AgentTypeCursor)
	}
}

func TestCursorAgent_Description(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	if ag.Description() == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCursorAgent_IsPreview(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	if !ag.IsPreview() {
		t.Error("IsPreview() = false, want true")
	}
}

func TestCursorAgent_ProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".cursor" {
		t.Errorf("ProtectedDirs() = %v, want [.cursor]", dirs)
	}
}

func TestCursorAgent_FormatResumeCommand(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	cmd := ag.FormatResumeCommand("some-session-id")
	if !strings.Contains(cmd, "Cursor") {
		t.Errorf("FormatResumeCommand() = %q, expected mention of Cursor", cmd)
	}
}

// --- GetSessionID ---

func TestCursorAgent_GetSessionID(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	input := &agent.HookInput{SessionID: "cursor-sess-42"}
	if id := ag.GetSessionID(input); id != "cursor-sess-42" {
		t.Errorf("GetSessionID() = %q, want cursor-sess-42", id)
	}
}

// --- ResolveSessionFile ---

func TestCursorAgent_ResolveSessionFile_FlatLayout(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	// When no nested dir exists but flat file exists, returns flat path (CLI pattern)
	tmpDir := t.TempDir()
	flatFile := filepath.Join(tmpDir, "abc123.jsonl")
	if err := os.WriteFile(flatFile, []byte("{}"), 0o644); err != nil {
		t.Fatalf("failed to write flat file: %v", err)
	}
	result, err := ag.ResolveSessionFile(tmpDir, "abc123")
	if err != nil {
		t.Fatalf("ResolveSessionFile() error = %v", err)
	}
	if result != flatFile {
		t.Errorf("ResolveSessionFile() flat = %q, want %q", result, flatFile)
	}
}

func TestCursorAgent_ResolveSessionFile_NeitherExists(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	// When neither nested nor flat file exists, returns flat path as best guess
	// (transcript may not exist yet at TurnStart time)
	tmpDir := t.TempDir()
	result, err := ag.ResolveSessionFile(tmpDir, "abc123")
	if err != nil {
		t.Fatalf("ResolveSessionFile() error = %v", err)
	}
	expected := filepath.Join(tmpDir, "abc123.jsonl")
	if result != expected {
		t.Errorf("ResolveSessionFile() neither = %q, want %q", result, expected)
	}
}

func TestCursorAgent_ResolveSessionFile_NestedLayout(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	// When nested dir and file exist, returns nested path (IDE pattern)
	tmpDir := t.TempDir()
	nestedDir := filepath.Join(tmpDir, "abc123")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("failed to create nested dir: %v", err)
	}
	nestedFile := filepath.Join(nestedDir, "abc123.jsonl")
	if err := os.WriteFile(nestedFile, []byte("{}"), 0o644); err != nil {
		t.Fatalf("failed to write nested file: %v", err)
	}

	result, err := ag.ResolveSessionFile(tmpDir, "abc123")
	if err != nil {
		t.Fatalf("ResolveSessionFile() error = %v", err)
	}
	if result != nestedFile {
		t.Errorf("ResolveSessionFile() nested = %q, want %q", result, nestedFile)
	}
}

func TestCursorAgent_ResolveSessionFile_NestedDirOnly(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	// When nested dir exists but file not yet flushed, predict nested path
	// (IDE creates the directory before the transcript file)
	tmpDir := t.TempDir()
	nestedDir := filepath.Join(tmpDir, "abc123")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("failed to create nested dir: %v", err)
	}

	result, err := ag.ResolveSessionFile(tmpDir, "abc123")
	if err != nil {
		t.Fatalf("ResolveSessionFile() error = %v", err)
	}
	expected := filepath.Join(nestedDir, "abc123.jsonl")
	if result != expected {
		t.Errorf("ResolveSessionFile() nested dir only = %q, want %q", result, expected)
	}
}

func TestCursorAgent_ResolveSessionFile_PrefersNested(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	// When both flat and nested exist, prefers nested
	tmpDir := t.TempDir()

	// Create flat file
	flatFile := filepath.Join(tmpDir, "abc123.jsonl")
	if err := os.WriteFile(flatFile, []byte("flat"), 0o644); err != nil {
		t.Fatalf("failed to write flat file: %v", err)
	}

	// Create nested file
	nestedDir := filepath.Join(tmpDir, "abc123")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("failed to create nested dir: %v", err)
	}
	nestedFile := filepath.Join(nestedDir, "abc123.jsonl")
	if err := os.WriteFile(nestedFile, []byte("nested"), 0o644); err != nil {
		t.Fatalf("failed to write nested file: %v", err)
	}

	result, err := ag.ResolveSessionFile(tmpDir, "abc123")
	if err != nil {
		t.Fatalf("ResolveSessionFile() error = %v", err)
	}
	if result != nestedFile {
		t.Errorf("ResolveSessionFile() should prefer nested = %q, got %q", nestedFile, result)
	}
}

// --- GetSessionDir ---

func TestCursorAgent_GetSessionDir_EnvOverride(t *testing.T) {
	ag := &CursorAgent{}
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", "/test/override")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if dir != "/test/override" {
		t.Errorf("GetSessionDir() = %q, want /test/override", dir)
	}
}

func TestCursorAgent_GetSessionDir_DefaultPath(t *testing.T) {
	ag := &CursorAgent{}
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", "")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("GetSessionDir() should return absolute path, got %q", dir)
	}
	if !strings.Contains(dir, ".cursor") {
		t.Errorf("GetSessionDir() = %q, expected path containing .cursor", dir)
	}
	if !strings.HasSuffix(dir, "agent-transcripts") {
		t.Errorf("GetSessionDir() = %q, expected path ending with agent-transcripts", dir)
	}
}

// --- ReadSession ---

func TestReadSession_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CursorAgent{}
	input := &agent.HookInput{
		SessionID:  "cursor-session-1",
		SessionRef: transcriptPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if session.SessionID != "cursor-session-1" {
		t.Errorf("SessionID = %q, want cursor-session-1", session.SessionID)
	}
	if session.AgentName != agent.AgentNameCursor {
		t.Errorf("AgentName = %q, want %q", session.AgentName, agent.AgentNameCursor)
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

	ag := &CursorAgent{}
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

func TestReadSession_ModifiedFilesEmpty(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CursorAgent{}
	input := &agent.HookInput{
		SessionID:  "sess-nofiles",
		SessionRef: transcriptPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	// Cursor transcripts don't contain tool_use blocks, so ModifiedFiles
	// should not be populated (file detection relies on git status instead).
	if len(session.ModifiedFiles) != 0 {
		t.Errorf("ModifiedFiles = %v, want empty (Cursor relies on git status)", session.ModifiedFiles)
	}
}

func TestReadSession_EmptySessionRef(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	input := &agent.HookInput{SessionID: "sess-no-ref"}

	_, err := ag.ReadSession(input)
	if err == nil {
		t.Fatal("ReadSession() should error when SessionRef is empty")
	}
}

func TestReadSession_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	input := &agent.HookInput{
		SessionID:  "sess-missing",
		SessionRef: "/nonexistent/path/transcript.jsonl",
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

	ag := &CursorAgent{}
	session := &agent.AgentSession{
		SessionID:  "write-session-1",
		AgentName:  agent.AgentNameCursor,
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

	ag := &CursorAgent{}

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
	ag := &CursorAgent{}
	if err := ag.WriteSession(context.Background(), nil); err == nil {
		t.Error("WriteSession(nil) should error")
	}
}

func TestWriteSession_WrongAgent(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
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

	ag := &CursorAgent{}
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
	ag := &CursorAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameCursor,
		NativeData: []byte("data"),
	}
	if err := ag.WriteSession(context.Background(), session); err == nil {
		t.Error("WriteSession() should error when SessionRef is empty")
	}
}

func TestWriteSession_NoNativeData(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameCursor,
		SessionRef: "/path/to/file",
	}
	if err := ag.WriteSession(context.Background(), session); err == nil {
		t.Error("WriteSession() should error when NativeData is empty")
	}
}

// --- ChunkTranscript / ReassembleTranscript ---

func TestChunkTranscript_SmallContent(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

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
	ag := &CursorAgent{}

	// Build content large enough to require chunking at a small maxSize
	var lines []string
	for i := range 20 {
		if i%2 == 0 {
			lines = append(lines, `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nmessage `+strings.Repeat("x", 100)+`\n</user_query>"}]}}`)
		} else {
			lines = append(lines, `{"role":"assistant","message":{"content":[{"type":"text","text":"response `+strings.Repeat("y", 100)+`"}]}}`)
		}
	}
	content := []byte(strings.Join(lines, "\n"))

	// Force chunking with a small max size
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
	ag := &CursorAgent{}

	// Build a multi-line JSONL transcript
	var lines []string
	for i := range 10 {
		if i%2 == 0 {
			lines = append(lines, `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nmsg-`+string(rune('A'+i))+`\n</user_query>"}]}}`)
		} else {
			lines = append(lines, `{"role":"assistant","message":{"content":[{"type":"text","text":"reply-`+string(rune('A'+i))+`"}]}}`)
		}
	}
	original := []byte(strings.Join(lines, "\n"))

	// Chunk with small max to force splits
	chunks, err := ag.ChunkTranscript(context.Background(), original, 300)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	// Reassemble
	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	if !bytes.Equal(original, reassembled) {
		t.Errorf("round-trip mismatch:\n  original len=%d\n  reassembled len=%d", len(original), len(reassembled))
	}
}

func TestChunkTranscript_SingleChunkRoundTrip(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	content := []byte(strings.Join(sampleTranscriptLines(), "\n"))

	chunks, err := ag.ChunkTranscript(context.Background(), content, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	if !bytes.Equal(content, reassembled) {
		t.Error("single-chunk round-trip should preserve content exactly")
	}
}

func TestChunkTranscript_EmptyContent(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

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
	ag := &CursorAgent{}

	result, err := ag.ReassembleTranscript([][]byte{})
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for empty chunks, got %d bytes", len(result))
	}
}

func TestChunkTranscript_PreservesLineOrder(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	// Create numbered lines for order verification
	var lines []string
	for i := range 20 {
		lines = append(lines, `{"role":"user","message":{"content":[{"type":"text","text":"line-`+
			strings.Repeat("0", 3-len(string(rune('0'+i/10))))+string(rune('0'+i/10))+string(rune('0'+i%10))+`"}]}}`)
	}
	original := strings.Join(lines, "\n")

	chunks, err := ag.ChunkTranscript(context.Background(), []byte(original), 400)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	if string(reassembled) != original {
		t.Error("chunk/reassemble did not preserve line order")
	}
}

// --- DetectPresence ---

func TestDetectPresence_NoCursorDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	ag := &CursorAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if present {
		t.Error("DetectPresence() = true, want false")
	}
}

func TestDetectPresence_WithCursorDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".cursor"), 0o755); err != nil {
		t.Fatalf("failed to create .cursor: %v", err)
	}

	// DetectPresence uses paths.RepoRoot(), which may not find a git repo.
	// Initialize one.
	initGitRepo(t, tmpDir)

	ag := &CursorAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if !present {
		t.Error("DetectPresence() = false, want true")
	}
}

// --- sanitizePathForCursor ---

func TestSanitizePathForCursor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		{"/Users/robin/project", "Users-robin-project"},
		{"/Users/robin/Developer/bingo", "Users-robin-Developer-bingo"},
		{"/tmp/test", "tmp-test"},
		{"simple", "simple"},
		{"/path/with spaces/dir", "path-with-spaces-dir"},
		{"/path.with.dots/dir", "path-with-dots-dir"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			result := sanitizePathForCursor(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizePathForCursor(%q) = %q, want %q", tt.input, result, tt.expected)
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
	// Minimal HEAD file so go-git / paths.RepoRoot() can find it
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("failed to write HEAD: %v", err)
	}
}
