package geminicli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"entire.io/cli/cmd/entire/cli/agent"
)

// Test constants
const testSessionID = "abc123"

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

func TestGetHookConfigPath(t *testing.T) {
	ag := &GeminiCLIAgent{}
	path := ag.GetHookConfigPath()
	if path != ".gemini/settings.json" {
		t.Errorf("GetHookConfigPath() = %q, want .gemini/settings.json", path)
	}
}

func TestSupportsHooks(t *testing.T) {
	ag := &GeminiCLIAgent{}
	if !ag.SupportsHooks() {
		t.Error("SupportsHooks() = false, want true")
	}
}

func TestParseHookInput_SessionStart(t *testing.T) {
	ag := &GeminiCLIAgent{}

	input := `{
		"session_id": "` + testSessionID + `",
		"transcript_path": "/path/to/transcript.json",
		"cwd": "/project",
		"hook_event_name": "session_start",
		"source": "startup"
	}`

	hookInput, err := ag.ParseHookInput(agent.HookSessionStart, bytes.NewReader([]byte(input)))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if hookInput.SessionID != testSessionID {
		t.Errorf("SessionID = %q, want %s", hookInput.SessionID, testSessionID)
	}
	if hookInput.SessionRef != "/path/to/transcript.json" {
		t.Errorf("SessionRef = %q, want /path/to/transcript.json", hookInput.SessionRef)
	}
	if hookInput.HookType != agent.HookSessionStart {
		t.Errorf("HookType = %v, want %v", hookInput.HookType, agent.HookSessionStart)
	}
}

func TestParseHookInput_SessionEnd(t *testing.T) {
	ag := &GeminiCLIAgent{}

	input := `{
		"session_id": "` + testSessionID + `",
		"transcript_path": "/path/to/transcript.json",
		"cwd": "/project",
		"hook_event_name": "session_end",
		"reason": "exit"
	}`

	hookInput, err := ag.ParseHookInput(agent.HookStop, bytes.NewReader([]byte(input)))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if hookInput.SessionID != testSessionID {
		t.Errorf("SessionID = %q, want %s", hookInput.SessionID, testSessionID)
	}
	if hookInput.RawData["reason"] != "exit" {
		t.Errorf("reason = %v, want exit", hookInput.RawData["reason"])
	}
}

func TestParseHookInput_PreToolUse(t *testing.T) {
	ag := &GeminiCLIAgent{}

	input := `{
		"session_id": "` + testSessionID + `",
		"transcript_path": "/path/to/transcript.json",
		"cwd": "/project",
		"hook_event_name": "before_tool",
		"tool_name": "write_file",
		"tool_input": {"file_path": "test.go", "content": "package main"}
	}`

	hookInput, err := ag.ParseHookInput(agent.HookPreToolUse, bytes.NewReader([]byte(input)))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if hookInput.ToolName != "write_file" {
		t.Errorf("ToolName = %q, want write_file", hookInput.ToolName)
	}
	if hookInput.ToolInput == nil {
		t.Error("ToolInput is nil")
	}
}

func TestParseHookInput_PostToolUse(t *testing.T) {
	ag := &GeminiCLIAgent{}

	input := `{
		"session_id": "` + testSessionID + `",
		"transcript_path": "/path/to/transcript.json",
		"cwd": "/project",
		"hook_event_name": "after_tool",
		"tool_name": "write_file",
		"tool_input": {"file_path": "test.go"},
		"tool_response": {"success": true}
	}`

	hookInput, err := ag.ParseHookInput(agent.HookPostToolUse, bytes.NewReader([]byte(input)))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if hookInput.ToolName != "write_file" {
		t.Errorf("ToolName = %q, want write_file", hookInput.ToolName)
	}
	if hookInput.ToolResponse == nil {
		t.Error("ToolResponse is nil")
	}
}

func TestParseHookInput_Empty(t *testing.T) {
	ag := &GeminiCLIAgent{}

	_, err := ag.ParseHookInput(agent.HookSessionStart, bytes.NewReader([]byte("")))
	if err == nil {
		t.Error("ParseHookInput() should error on empty input")
	}
}

func TestParseHookInput_InvalidJSON(t *testing.T) {
	ag := &GeminiCLIAgent{}

	_, err := ag.ParseHookInput(agent.HookSessionStart, bytes.NewReader([]byte("not json")))
	if err == nil {
		t.Error("ParseHookInput() should error on invalid JSON")
	}
}

func TestGetSessionID(t *testing.T) {
	ag := &GeminiCLIAgent{}
	input := &agent.HookInput{SessionID: "test-session-123"}

	id := ag.GetSessionID(input)
	if id != "test-session-123" {
		t.Errorf("GetSessionID() = %q, want test-session-123", id)
	}
}

func TestTransformSessionID(t *testing.T) {
	ag := &GeminiCLIAgent{}

	// TransformSessionID should add date prefix
	result := ag.TransformSessionID("abc123")
	if result == "abc123" {
		t.Error("TransformSessionID() should add date prefix")
	}
	if len(result) < len("abc123")+11 { // 11 chars for "YYYY-MM-DD-"
		t.Errorf("TransformSessionID() result too short: %q", result)
	}
}

func TestExtractAgentSessionID(t *testing.T) {
	ag := &GeminiCLIAgent{}

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

func TestSanitizePathForGemini(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Users/test/project", "-Users-test-project"},
		{"simple", "simple"},
		{"/path/with spaces/dir", "-path-with-spaces-dir"},
	}

	for _, tt := range tests {
		got := SanitizePathForGemini(tt.input)
		if got != tt.want {
			t.Errorf("SanitizePathForGemini(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGetSupportedHooks(t *testing.T) {
	ag := &GeminiCLIAgent{}
	hooks := ag.GetSupportedHooks()

	expected := []agent.HookType{
		agent.HookSessionStart,
		agent.HookStop,             // Maps to Gemini's SessionEnd
		agent.HookUserPromptSubmit, // Maps to Gemini's BeforeAgent
		agent.HookPreToolUse,       // Maps to Gemini's BeforeTool
		agent.HookPostToolUse,      // Maps to Gemini's AfterTool
	}

	if len(hooks) != len(expected) {
		t.Errorf("GetSupportedHooks() returned %d hooks, want %d", len(hooks), len(expected))
	}

	for i, hook := range expected {
		if hooks[i] != hook {
			t.Errorf("GetSupportedHooks()[%d] = %v, want %v", i, hooks[i], hook)
		}
	}
}
