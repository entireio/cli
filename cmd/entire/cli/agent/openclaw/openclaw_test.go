package openclaw

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestNewOpenClawAgent(t *testing.T) {
	ag := NewOpenClawAgent()
	if ag == nil {
		t.Fatal("NewOpenClawAgent() returned nil")
	}

	oc, ok := ag.(*OpenClawAgent)
	if !ok {
		t.Fatal("NewOpenClawAgent() didn't return *OpenClawAgent")
	}
	if oc == nil {
		t.Fatal("NewOpenClawAgent() returned nil agent")
	}
}

func TestName(t *testing.T) {
	ag := &OpenClawAgent{}
	if name := ag.Name(); name != agent.AgentNameOpenClaw {
		t.Errorf("Name() = %q, want %q", name, agent.AgentNameOpenClaw)
	}
}

func TestType(t *testing.T) {
	ag := &OpenClawAgent{}
	if agType := ag.Type(); agType != agent.AgentTypeOpenClaw {
		t.Errorf("Type() = %q, want %q", agType, agent.AgentTypeOpenClaw)
	}
}

func TestDescription(t *testing.T) {
	ag := &OpenClawAgent{}
	desc := ag.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestDetectPresence_NoOpenClawDir(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Clear env var
	t.Setenv("OPENCLAW_SESSION", "")

	ag := &OpenClawAgent{}
	present, err := ag.DetectPresence()
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if present {
		t.Error("DetectPresence() = true, want false")
	}
}

func TestDetectPresence_WithOpenClawDir(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Clear env var
	t.Setenv("OPENCLAW_SESSION", "")

	// Create .openclaw directory
	if err := os.Mkdir(".openclaw", 0o755); err != nil {
		t.Fatalf("failed to create .openclaw: %v", err)
	}

	ag := &OpenClawAgent{}
	present, err := ag.DetectPresence()
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if !present {
		t.Error("DetectPresence() = false, want true")
	}
}

func TestDetectPresence_WithEnvVar(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	t.Setenv("OPENCLAW_SESSION", "test-session-123")

	ag := &OpenClawAgent{}
	present, err := ag.DetectPresence()
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if !present {
		t.Error("DetectPresence() = false, want true (env var set)")
	}
}

func TestGetHookConfigPath(t *testing.T) {
	ag := &OpenClawAgent{}
	path := ag.GetHookConfigPath()
	if path != "" {
		t.Errorf("GetHookConfigPath() = %q, want empty (OpenClaw uses git hooks)", path)
	}
}

func TestSupportsHooks(t *testing.T) {
	ag := &OpenClawAgent{}
	if ag.SupportsHooks() {
		t.Error("SupportsHooks() = true, want false (OpenClaw uses git hooks)")
	}
}

func TestParseHookInput(t *testing.T) {
	ag := &OpenClawAgent{}

	input := `{"session_id":"test-123","transcript_path":"/tmp/transcript.jsonl"}`

	result, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "test-123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "test-123")
	}
	if result.SessionRef != "/tmp/transcript.jsonl" {
		t.Errorf("SessionRef = %q, want %q", result.SessionRef, "/tmp/transcript.jsonl")
	}
}

func TestParseHookInput_Empty(t *testing.T) {
	ag := &OpenClawAgent{}

	_, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader(""))
	if err == nil {
		t.Error("ParseHookInput() should error on empty input")
	}
}

func TestParseHookInput_InvalidJSON(t *testing.T) {
	ag := &OpenClawAgent{}

	_, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader("not json"))
	if err == nil {
		t.Error("ParseHookInput() should error on invalid JSON")
	}
}

func TestGetSessionID(t *testing.T) {
	ag := &OpenClawAgent{}
	input := &agent.HookInput{SessionID: "test-session-123"}

	id := ag.GetSessionID(input)
	if id != "test-session-123" {
		t.Errorf("GetSessionID() = %q, want test-session-123", id)
	}
}

func TestTransformSessionID(t *testing.T) {
	ag := &OpenClawAgent{}

	// TransformSessionID is an identity function
	result := ag.TransformSessionID("abc123")
	if result != "abc123" {
		t.Errorf("TransformSessionID() = %q, want abc123 (identity function)", result)
	}
}

func TestExtractAgentSessionID(t *testing.T) {
	ag := &OpenClawAgent{}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "with date prefix",
			input: "2025-01-09-abc123",
			want:  "abc123",
		},
		{
			name:  "without date prefix",
			input: "abc123",
			want:  "abc123",
		},
		{
			name:  "longer session id",
			input: "2025-12-31-session-id-here",
			want:  "session-id-here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ag.ExtractAgentSessionID(tt.input)
			if got != tt.want {
				t.Errorf("ExtractAgentSessionID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetSessionDir(t *testing.T) {
	ag := &OpenClawAgent{}

	// Test with override env var
	t.Setenv("ENTIRE_TEST_OPENCLAW_SESSION_DIR", "/test/override")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if dir != "/test/override" {
		t.Errorf("GetSessionDir() = %q, want /test/override", dir)
	}
}

func TestGetSessionDir_WithOpenClawEnv(t *testing.T) {
	ag := &OpenClawAgent{}

	t.Setenv("ENTIRE_TEST_OPENCLAW_SESSION_DIR", "")
	t.Setenv("OPENCLAW_SESSION_DIR", "/custom/sessions")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if dir != "/custom/sessions" {
		t.Errorf("GetSessionDir() = %q, want /custom/sessions", dir)
	}
}

func TestGetSessionDir_DefaultPath(t *testing.T) {
	ag := &OpenClawAgent{}

	// Make sure env vars are not set
	t.Setenv("ENTIRE_TEST_OPENCLAW_SESSION_DIR", "")
	t.Setenv("OPENCLAW_SESSION_DIR", "")

	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}

	// Should be an absolute path ending with .openclaw/sessions
	if !filepath.IsAbs(dir) {
		t.Errorf("GetSessionDir() should return absolute path, got %q", dir)
	}
	if !strings.HasSuffix(dir, filepath.Join(".openclaw", "sessions")) {
		t.Errorf("GetSessionDir() = %q, want suffix .openclaw/sessions", dir)
	}
}

func TestFormatResumeCommand(t *testing.T) {
	ag := &OpenClawAgent{}

	cmd := ag.FormatResumeCommand("abc123")
	expected := "openclaw resume abc123"
	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

func TestReadSession(t *testing.T) {
	tempDir := t.TempDir()

	// Create a transcript file
	transcriptPath := filepath.Join(tempDir, "transcript.jsonl")
	transcriptContent := `{"role":"user","content":"hello","timestamp":"2025-01-01T00:00:00Z"}
{"role":"assistant","content":"hi there","timestamp":"2025-01-01T00:00:01Z"}
`
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &OpenClawAgent{}
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
	if session.AgentName != agent.AgentNameOpenClaw {
		t.Errorf("AgentName = %q, want %q", session.AgentName, agent.AgentNameOpenClaw)
	}
	if len(session.NativeData) == 0 {
		t.Error("NativeData is empty")
	}
}

func TestReadSession_NoSessionRef(t *testing.T) {
	ag := &OpenClawAgent{}
	input := &agent.HookInput{SessionID: "test-session"}

	_, err := ag.ReadSession(input)
	if err == nil {
		t.Error("ReadSession() should error when SessionRef is empty")
	}
}

func TestReadSession_WithModifiedFiles(t *testing.T) {
	tempDir := t.TempDir()

	transcriptPath := filepath.Join(tempDir, "transcript.jsonl")
	transcriptContent := `{"role":"user","content":"write a file","timestamp":"2025-01-01T00:00:00Z"}
{"role":"assistant","content":"writing...","tool_calls":[{"name":"write","params":{"file_path":"main.go"}}],"timestamp":"2025-01-01T00:00:01Z"}
{"role":"assistant","content":"editing...","tool_calls":[{"name":"edit","params":{"file_path":"utils.go"}}],"timestamp":"2025-01-01T00:00:02Z"}
`
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &OpenClawAgent{}
	input := &agent.HookInput{
		SessionID:  "test-session",
		SessionRef: transcriptPath,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if len(session.ModifiedFiles) != 2 {
		t.Fatalf("ModifiedFiles count = %d, want 2", len(session.ModifiedFiles))
	}

	hasFile := func(name string) bool {
		for _, f := range session.ModifiedFiles {
			if f == name {
				return true
			}
		}
		return false
	}

	if !hasFile("main.go") {
		t.Error("ModifiedFiles missing main.go")
	}
	if !hasFile("utils.go") {
		t.Error("ModifiedFiles missing utils.go")
	}
}

func TestWriteSession(t *testing.T) {
	tempDir := t.TempDir()
	transcriptPath := filepath.Join(tempDir, "transcript.jsonl")

	ag := &OpenClawAgent{}
	session := &agent.AgentSession{
		SessionID:  "test-session",
		AgentName:  agent.AgentNameOpenClaw,
		SessionRef: transcriptPath,
		NativeData: []byte(`{"role":"user","content":"hello"}` + "\n"),
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

	if string(data) != `{"role":"user","content":"hello"}`+"\n" {
		t.Errorf("transcript content = %q, want JSONL data", string(data))
	}
}

func TestWriteSession_CreatesDirectory(t *testing.T) {
	tempDir := t.TempDir()
	transcriptPath := filepath.Join(tempDir, "nested", "dir", "transcript.jsonl")

	ag := &OpenClawAgent{}
	session := &agent.AgentSession{
		SessionID:  "test-session",
		AgentName:  agent.AgentNameOpenClaw,
		SessionRef: transcriptPath,
		NativeData: []byte(`{"role":"user","content":"hello"}` + "\n"),
	}

	err := ag.WriteSession(session)
	if err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(transcriptPath); err != nil {
		t.Errorf("transcript file not created: %v", err)
	}
}

func TestWriteSession_Nil(t *testing.T) {
	ag := &OpenClawAgent{}

	err := ag.WriteSession(nil)
	if err == nil {
		t.Error("WriteSession(nil) should error")
	}
}

func TestWriteSession_WrongAgent(t *testing.T) {
	ag := &OpenClawAgent{}
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
	ag := &OpenClawAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameOpenClaw,
		NativeData: []byte("{}"),
	}

	err := ag.WriteSession(session)
	if err == nil {
		t.Error("WriteSession() should error when SessionRef is empty")
	}
}

func TestWriteSession_NoNativeData(t *testing.T) {
	ag := &OpenClawAgent{}
	session := &agent.AgentSession{
		AgentName:  agent.AgentNameOpenClaw,
		SessionRef: "/path/to/file",
	}

	err := ag.WriteSession(session)
	if err == nil {
		t.Error("WriteSession() should error when NativeData is empty")
	}
}

// Transcript parsing tests

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	data := []byte(`{"role":"user","content":"hello","timestamp":"2025-01-01T00:00:00Z"}
{"role":"assistant","content":"hi there","timestamp":"2025-01-01T00:00:01Z"}
`)

	messages, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(messages) != 2 {
		t.Errorf("ParseTranscript() got %d messages, want 2", len(messages))
	}

	if messages[0].Role != "user" || messages[0].Content != "hello" {
		t.Errorf("First message = %+v, want role=user, content=hello", messages[0])
	}

	if messages[1].Role != "assistant" || messages[1].Content != "hi there" {
		t.Errorf("Second message = %+v, want role=assistant, content=hi there", messages[1])
	}
}

func TestParseTranscript_SkipsMalformed(t *testing.T) {
	t.Parallel()

	data := []byte(`{"role":"user","content":"hello"}
not valid json
{"role":"assistant","content":"hi"}
`)

	messages, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(messages) != 2 {
		t.Errorf("ParseTranscript() got %d messages, want 2 (skipping malformed)", len(messages))
	}
}

func TestSerializeTranscript(t *testing.T) {
	t.Parallel()

	messages := []OpenClawMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}

	data, err := SerializeTranscript(messages)
	if err != nil {
		t.Fatalf("SerializeTranscript() error = %v", err)
	}

	// Parse back to verify round-trip
	parsed, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript(serialized) error = %v", err)
	}

	if len(parsed) != 2 {
		t.Errorf("Round-trip got %d messages, want 2", len(parsed))
	}

	if parsed[0].Role != "user" || parsed[0].Content != "hello" {
		t.Errorf("Round-trip first message = %+v", parsed[0])
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	messages := []OpenClawMessage{
		{Role: "assistant", Content: "writing...", ToolCalls: []OpenClawToolCall{
			{Name: "write", Params: map[string]interface{}{"file_path": "foo.go"}},
		}},
		{Role: "assistant", Content: "editing...", ToolCalls: []OpenClawToolCall{
			{Name: "edit", Params: map[string]interface{}{"file_path": "bar.go"}},
		}},
		{Role: "assistant", Content: "running...", ToolCalls: []OpenClawToolCall{
			{Name: "exec", Params: map[string]interface{}{"command": "ls"}},
		}},
		{Role: "assistant", Content: "writing again...", ToolCalls: []OpenClawToolCall{
			{Name: "write", Params: map[string]interface{}{"file_path": "foo.go"}},
		}},
	}

	files := ExtractModifiedFiles(messages)

	// Should have foo.go and bar.go (deduplicated, exec not included)
	if len(files) != 2 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 2", len(files))
	}

	hasFile := func(name string) bool {
		for _, f := range files {
			if f == name {
				return true
			}
		}
		return false
	}

	if !hasFile("foo.go") {
		t.Error("ExtractModifiedFiles() missing foo.go")
	}
	if !hasFile("bar.go") {
		t.Error("ExtractModifiedFiles() missing bar.go")
	}
}

func TestExtractModifiedFiles_PathParam(t *testing.T) {
	t.Parallel()

	messages := []OpenClawMessage{
		{Role: "assistant", ToolCalls: []OpenClawToolCall{
			{Name: "write", Params: map[string]interface{}{"path": "main.go"}},
		}},
	}

	files := ExtractModifiedFiles(messages)
	if len(files) != 1 || files[0] != "main.go" {
		t.Errorf("ExtractModifiedFiles() = %v, want [main.go]", files)
	}
}

func TestExtractLastUserPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []OpenClawMessage
		want     string
	}{
		{
			name: "multiple messages",
			messages: []OpenClawMessage{
				{Role: "user", Content: "first"},
				{Role: "assistant", Content: "response"},
				{Role: "user", Content: "second"},
			},
			want: "second",
		},
		{
			name:     "empty transcript",
			messages: nil,
			want:     "",
		},
		{
			name: "no user messages",
			messages: []OpenClawMessage{
				{Role: "assistant", Content: "hello"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractLastUserPrompt(tt.messages)
			if got != tt.want {
				t.Errorf("ExtractLastUserPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TranscriptAnalyzer tests

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()

	ag := &OpenClawAgent{}

	t.Run("empty path", func(t *testing.T) {
		pos, err := ag.GetTranscriptPosition("")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if pos != 0 {
			t.Errorf("position = %d, want 0", pos)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		pos, err := ag.GetTranscriptPosition("/nonexistent/path")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if pos != 0 {
			t.Errorf("position = %d, want 0", pos)
		}
	})

	t.Run("file with lines", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "transcript.jsonl")
		content := `{"role":"user","content":"hello"}
{"role":"assistant","content":"hi"}
{"role":"user","content":"bye"}
`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}

		pos, err := ag.GetTranscriptPosition(path)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if pos != 3 {
			t.Errorf("position = %d, want 3", pos)
		}
	})
}

func TestExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "transcript.jsonl")

	content := `{"role":"user","content":"write foo"}
{"role":"assistant","tool_calls":[{"name":"write","params":{"file_path":"foo.go"}}]}
{"role":"user","content":"write bar"}
{"role":"assistant","tool_calls":[{"name":"write","params":{"file_path":"bar.go"}}]}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	ag := &OpenClawAgent{}

	// From offset 0 - should get both files
	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if pos != 4 {
		t.Errorf("position = %d, want 4", pos)
	}
	if len(files) != 2 {
		t.Errorf("files count = %d, want 2", len(files))
	}

	// From offset 2 - should get only bar.go
	files, pos, err = ag.ExtractModifiedFilesFromOffset(path, 2)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if pos != 4 {
		t.Errorf("position = %d, want 4", pos)
	}
	if len(files) != 1 || files[0] != "bar.go" {
		t.Errorf("files = %v, want [bar.go]", files)
	}
}

// Chunking tests

func TestChunkTranscript_SmallContent(t *testing.T) {
	ag := &OpenClawAgent{}

	content := []byte(`{"role":"user","content":"hello"}
{"role":"assistant","content":"hi"}
`)

	chunks, err := ag.ChunkTranscript(content, agent.MaxChunkSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk, got %d", len(chunks))
	}
}

func TestChunkTranscript_LargeContent(t *testing.T) {
	ag := &OpenClawAgent{}

	// Create content that exceeds maxSize
	var lines []string
	for i := range 100 {
		msg := OpenClawMessage{
			Role:    "user",
			Content: fmt.Sprintf("message %d: %s", i, strings.Repeat("x", 500)),
		}
		data, _ := json.Marshal(msg)
		lines = append(lines, string(data))
	}
	content := []byte(strings.Join(lines, "\n") + "\n")

	maxSize := 5000
	chunks, err := ag.ChunkTranscript(content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	if len(chunks) < 2 {
		t.Errorf("Expected at least 2 chunks for large content, got %d", len(chunks))
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	ag := &OpenClawAgent{}

	messages := []OpenClawMessage{
		{Role: "user", Content: "Write a hello world program"},
		{Role: "assistant", Content: "Sure!", ToolCalls: []OpenClawToolCall{
			{Name: "write", Params: map[string]interface{}{"file_path": "main.go", "content": "package main"}},
		}},
		{Role: "user", Content: "Now add a function"},
		{Role: "assistant", Content: "Done!", ToolCalls: []OpenClawToolCall{
			{Name: "edit", Params: map[string]interface{}{"file_path": "main.go"}},
		}},
	}

	content, err := SerializeTranscript(messages)
	if err != nil {
		t.Fatalf("SerializeTranscript() error = %v", err)
	}

	// Small maxSize to force chunking
	maxSize := 200
	chunks, err := ag.ChunkTranscript(content, maxSize)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	// Parse and verify
	result, err := ParseTranscript(reassembled)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(result) != len(messages) {
		t.Fatalf("Message count mismatch: got %d, want %d", len(result), len(messages))
	}

	for i, msg := range result {
		if msg.Role != messages[i].Role {
			t.Errorf("Message %d role = %q, want %q", i, msg.Role, messages[i].Role)
		}
		if msg.Content != messages[i].Content {
			t.Errorf("Message %d content = %q, want %q", i, msg.Content, messages[i].Content)
		}
	}
}

func TestReassembleTranscript_EmptyChunks(t *testing.T) {
	ag := &OpenClawAgent{}

	result, err := ag.ReassembleTranscript([][]byte{})
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}
	if len(result) != 0 {
		t.Errorf("Expected empty result for empty chunks, got %d bytes", len(result))
	}
}

// Registration test

func TestAgentRegistered(t *testing.T) {
	ag, err := agent.Get(agent.AgentNameOpenClaw)
	if err != nil {
		t.Fatalf("agent.Get(openclaw) error = %v", err)
	}
	if ag == nil {
		t.Fatal("agent.Get(openclaw) returned nil")
	}
	if ag.Name() != agent.AgentNameOpenClaw {
		t.Errorf("registered agent name = %q, want %q", ag.Name(), agent.AgentNameOpenClaw)
	}
	if ag.Type() != agent.AgentTypeOpenClaw {
		t.Errorf("registered agent type = %q, want %q", ag.Type(), agent.AgentTypeOpenClaw)
	}
}
