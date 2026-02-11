package pi

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestNewPiAgent(t *testing.T) {
	ag := NewPiAgent()
	if ag == nil {
		t.Fatal("NewPiAgent() returned nil")
	}
	if _, ok := ag.(*PiAgent); !ok {
		t.Fatalf("NewPiAgent() returned %T, want *PiAgent", ag)
	}
}

func TestNameTypeDescription(t *testing.T) {
	ag := &PiAgent{}

	if got := ag.Name(); got != agent.AgentNamePi {
		t.Fatalf("Name() = %q, want %q", got, agent.AgentNamePi)
	}
	if got := ag.Type(); got != agent.AgentTypePi {
		t.Fatalf("Type() = %q, want %q", got, agent.AgentTypePi)
	}
	if got := ag.Description(); got == "" {
		t.Fatal("Description() returned empty string")
	}
}

func TestDetectPresence(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		wanted bool
	}{
		{
			name: "no .pi directory",
			setup: func(_ *testing.T, _ string) {
				// nothing
			},
			wanted: false,
		},
		{
			name: "with .pi directory",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(dir, ".pi"), 0o755); err != nil {
					t.Fatalf("failed to create .pi directory: %v", err)
				}
			},
			wanted: true,
		},
		{
			name: "with .pi/settings.json",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				settingsPath := filepath.Join(dir, ".pi", "settings.json")
				if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
					t.Fatalf("failed to create .pi directory: %v", err)
				}
				if err := os.WriteFile(settingsPath, []byte(`{"packages":[]}`), 0o644); err != nil {
					t.Fatalf("failed to write settings.json: %v", err)
				}
			},
			wanted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			tt.setup(t, dir)

			ag := &PiAgent{}
			present, err := ag.DetectPresence()
			if err != nil {
				t.Fatalf("DetectPresence() error = %v", err)
			}
			if present != tt.wanted {
				t.Fatalf("DetectPresence() = %v, want %v", present, tt.wanted)
			}
		})
	}
}

func TestGetHookConfigPathAndSupportsHooks(t *testing.T) {
	ag := &PiAgent{}
	if got := ag.GetHookConfigPath(); got != "" {
		t.Fatalf("GetHookConfigPath() = %q, want empty string", got)
	}
	if !ag.SupportsHooks() {
		t.Fatal("SupportsHooks() = false, want true")
	}
}

func TestParseHookInput(t *testing.T) {
	ag := &PiAgent{}
	input := `{"session_id":"sess-1","transcript_path":"/tmp/session.jsonl","cwd":"/repo/path","prompt":"Fix it","modified_files":["a.go","b.go"],"leaf_id":"leaf-123"}`

	hookInput, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if hookInput.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", hookInput.SessionID)
	}
	if hookInput.SessionRef != "/tmp/session.jsonl" {
		t.Fatalf("SessionRef = %q, want /tmp/session.jsonl", hookInput.SessionRef)
	}
	if hookInput.UserPrompt != "Fix it" {
		t.Fatalf("UserPrompt = %q, want Fix it", hookInput.UserPrompt)
	}
	if hookInput.HookType != agent.HookStop {
		t.Fatalf("HookType = %v, want %v", hookInput.HookType, agent.HookStop)
	}
	if hookInput.RawData == nil {
		t.Fatalf("RawData is nil")
	}
	if _, ok := hookInput.RawData["modified_files"]; !ok {
		t.Fatalf("RawData[modified_files] missing")
	}
	if got := hookInput.RawData["leaf_id"]; got != "leaf-123" {
		t.Fatalf("RawData[leaf_id] = %v, want leaf-123", got)
	}
	if got := hookInput.RawData["cwd"]; got != "/repo/path" {
		t.Fatalf("RawData[cwd] = %v, want /repo/path", got)
	}
}

func TestParseHookInput_EmptyInput(t *testing.T) {
	ag := &PiAgent{}
	_, err := ag.ParseHookInput(agent.HookSessionStart, bytes.NewBuffer(nil))
	if err == nil {
		t.Fatal("ParseHookInput() expected error for empty input")
	}
}

func TestParseHookInput_InvalidJSON(t *testing.T) {
	ag := &PiAgent{}
	_, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader("not-json"))
	if err == nil {
		t.Fatal("ParseHookInput() expected error for invalid JSON")
	}
}

func TestParseHookInput_SessionStartAndSessionEnd(t *testing.T) {
	ag := &PiAgent{}

	startInput, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader(`{"session_id":"s-start","transcript_path":"/tmp/start.jsonl"}`))
	if err != nil {
		t.Fatalf("ParseHookInput(session-start) error = %v", err)
	}
	if startInput.HookType != agent.HookSessionStart {
		t.Fatalf("HookType = %v, want %v", startInput.HookType, agent.HookSessionStart)
	}
	if startInput.SessionID != "s-start" {
		t.Fatalf("SessionID = %q, want s-start", startInput.SessionID)
	}

	endInput, err := ag.ParseHookInput(agent.HookSessionEnd, strings.NewReader(`{"session_id":"s-end","transcript_path":"/tmp/end.jsonl"}`))
	if err != nil {
		t.Fatalf("ParseHookInput(session-end) error = %v", err)
	}
	if endInput.HookType != agent.HookSessionEnd {
		t.Fatalf("HookType = %v, want %v", endInput.HookType, agent.HookSessionEnd)
	}
	if endInput.SessionID != "s-end" {
		t.Fatalf("SessionID = %q, want s-end", endInput.SessionID)
	}
}

func TestParseHookInput_BeforeAndAfterTool(t *testing.T) {
	ag := &PiAgent{}

	beforeInput, err := ag.ParseHookInput(agent.HookPreToolUse, strings.NewReader(`{"session_id":"s-tool","transcript_path":"/tmp/t.jsonl","tool_name":"write","tool_use_id":"tool-1","tool_input":{"path":"a.txt"}}`))
	if err != nil {
		t.Fatalf("ParseHookInput(before-tool) error = %v", err)
	}
	if beforeInput.ToolName != "write" {
		t.Fatalf("ToolName = %q, want write", beforeInput.ToolName)
	}
	if beforeInput.ToolUseID != "tool-1" {
		t.Fatalf("ToolUseID = %q, want tool-1", beforeInput.ToolUseID)
	}
	if len(beforeInput.ToolInput) == 0 {
		t.Fatalf("ToolInput should be populated")
	}

	afterInput, err := ag.ParseHookInput(agent.HookPostToolUse, strings.NewReader(`{"session_id":"s-tool","transcript_path":"/tmp/t.jsonl","tool_name":"write","tool_use_id":"tool-1","tool_input":{"path":"a.txt"},"tool_response":{"ok":true}}`))
	if err != nil {
		t.Fatalf("ParseHookInput(after-tool) error = %v", err)
	}
	if len(afterInput.ToolResponse) == 0 {
		t.Fatalf("ToolResponse should be populated for post-tool hook")
	}
}

func TestGetSessionIDTransformations(t *testing.T) {
	ag := &PiAgent{}

	const sessionID = "abc-123"

	hookInput := &agent.HookInput{SessionID: sessionID}
	if got := ag.GetSessionID(hookInput); got != sessionID {
		t.Fatalf("GetSessionID() = %q, want %q", got, sessionID)
	}

	if got := ag.TransformSessionID(sessionID); got != sessionID {
		t.Fatalf("TransformSessionID() = %q, want %q", got, sessionID)
	}

	if got := ag.ExtractAgentSessionID(sessionID); got != sessionID {
		t.Fatalf("ExtractAgentSessionID() = %q, want %q", got, sessionID)
	}
}

func TestGetSessionDir(t *testing.T) {
	ag := &PiAgent{}
	got, err := ag.GetSessionDir("/repo/project")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}

	if !filepath.IsAbs(got) {
		t.Fatalf("GetSessionDir() should return absolute path, got %q", got)
	}
	if !strings.Contains(got, filepath.Join(".pi", "agent", "sessions")) {
		t.Fatalf("GetSessionDir() = %q, expected .pi/agent/sessions in path", got)
	}
	if !strings.HasSuffix(got, "--repo-project--") {
		t.Fatalf("GetSessionDir() = %q, expected suffix --repo-project--", got)
	}
}

func TestResolveSessionFile(t *testing.T) {
	ag := &PiAgent{}
	got := ag.ResolveSessionFile("/tmp/pi/sessions", "session-123")
	want := filepath.Join("/tmp/pi/sessions", "session-123.jsonl")
	if got != want {
		t.Fatalf("ResolveSessionFile() = %q, want %q", got, want)
	}
}

func TestProtectedDirs(t *testing.T) {
	ag := &PiAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".pi" {
		t.Fatalf("ProtectedDirs() = %#v, want [.pi]", dirs)
	}
}

func TestReadSession(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "session.jsonl")
	content := `{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","arguments":{"path":"foo.go"}}]}}
`
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &PiAgent{}
	session, err := ag.ReadSession(&agent.HookInput{SessionID: "s1", SessionRef: transcriptPath})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if session.SessionID != "s1" {
		t.Fatalf("SessionID = %q, want s1", session.SessionID)
	}
	if session.AgentName != agent.AgentNamePi {
		t.Fatalf("AgentName = %q, want %q", session.AgentName, agent.AgentNamePi)
	}
	if len(session.NativeData) == 0 {
		t.Fatalf("NativeData should not be empty")
	}
	if len(session.ModifiedFiles) != 1 || session.ModifiedFiles[0] != "foo.go" {
		t.Fatalf("ModifiedFiles = %#v, want [foo.go]", session.ModifiedFiles)
	}
}

func TestReadSession_Errors(t *testing.T) {
	ag := &PiAgent{}

	if _, err := ag.ReadSession(&agent.HookInput{SessionID: "s1"}); err == nil {
		t.Fatal("ReadSession() expected error when SessionRef is empty")
	}

	if _, err := ag.ReadSession(&agent.HookInput{SessionID: "s1", SessionRef: "/does/not/exist"}); err == nil {
		t.Fatal("ReadSession() expected error for missing file")
	}
}

func TestWriteSession(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "session.jsonl")

	ag := &PiAgent{}
	session := &agent.AgentSession{
		SessionID:  "s1",
		AgentName:  agent.AgentNamePi,
		SessionRef: transcriptPath,
		NativeData: []byte(`{"type":"message","id":"1"}`),
	}

	if err := ag.WriteSession(session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read transcript file: %v", err)
	}
	if string(data) != string(session.NativeData) {
		t.Fatalf("written data mismatch: got %q want %q", string(data), string(session.NativeData))
	}
}

func TestWriteSession_Errors(t *testing.T) {
	ag := &PiAgent{}

	if err := ag.WriteSession(nil); err == nil {
		t.Fatal("WriteSession(nil) expected error")
	}

	if err := ag.WriteSession(&agent.AgentSession{AgentName: agent.AgentNameClaudeCode, SessionRef: "x", NativeData: []byte("x")}); err == nil {
		t.Fatal("WriteSession() expected error for wrong agent")
	}

	if err := ag.WriteSession(&agent.AgentSession{AgentName: agent.AgentNamePi, NativeData: []byte("x")}); err == nil {
		t.Fatal("WriteSession() expected error for missing SessionRef")
	}

	if err := ag.WriteSession(&agent.AgentSession{AgentName: agent.AgentNamePi, SessionRef: "x"}); err == nil {
		t.Fatal("WriteSession() expected error for empty NativeData")
	}
}

func TestFormatResumeCommand(t *testing.T) {
	ag := &PiAgent{}
	cmd := ag.FormatResumeCommand("abc")
	if !strings.Contains(cmd, "pi") {
		t.Fatalf("FormatResumeCommand() = %q, expected to contain 'pi'", cmd)
	}
}

func TestGetLastUserPrompt(t *testing.T) {
	ag := &PiAgent{}
	session := &agent.AgentSession{
		NativeData: []byte(`{"type":"message","id":"1","message":{"role":"user","content":"first"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"message","id":"3","message":{"role":"user","content":[{"type":"text","text":"second"},{"type":"text","text":"prompt"}]}}
`),
	}

	got := ag.GetLastUserPrompt(session)
	if got != "second\n\nprompt" {
		t.Fatalf("GetLastUserPrompt() = %q, want %q", got, "second\n\nprompt")
	}
}

func TestTruncateAtUUID(t *testing.T) {
	ag := &PiAgent{}
	session := &agent.AgentSession{
		SessionID:  "s1",
		AgentName:  agent.AgentNamePi,
		SessionRef: "/tmp/x",
		NativeData: []byte(`{"type":"message","id":"1","message":{"role":"user","content":"one"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}
{"type":"message","uuid":"3","message":{"role":"assistant","content":[{"type":"text","text":"three"}]}}
`),
	}

	truncated, err := ag.TruncateAtUUID(session, "2")
	if err != nil {
		t.Fatalf("TruncateAtUUID() error = %v", err)
	}

	if strings.Count(string(truncated.NativeData), "\n") != 2 {
		t.Fatalf("expected 2 lines after truncation, got content:\n%s", string(truncated.NativeData))
	}

	truncatedByUUID, err := ag.TruncateAtUUID(session, "3")
	if err != nil {
		t.Fatalf("TruncateAtUUID(uuid) error = %v", err)
	}
	if strings.Count(string(truncatedByUUID.NativeData), "\n") != 3 {
		t.Fatalf("expected 3 lines when truncating by uuid, got content:\n%s", string(truncatedByUUID.NativeData))
	}

	copied, err := ag.TruncateAtUUID(session, "")
	if err != nil {
		t.Fatalf("TruncateAtUUID(empty) error = %v", err)
	}
	if string(copied.NativeData) != string(session.NativeData) {
		t.Fatalf("empty truncation should return unchanged session")
	}
}

func TestTruncateAtUUID_TreePath(t *testing.T) {
	ag := &PiAgent{}
	session := &agent.AgentSession{
		SessionID:  "s-tree",
		AgentName:  agent.AgentNamePi,
		SessionRef: "/tmp/x",
		NativeData: []byte(`{"type":"session","version":3,"id":"sess","timestamp":"2026-01-01T00:00:00Z","cwd":"/repo"}
{"type":"message","id":"1","parentId":null,"message":{"role":"user","content":"root"}}
{"type":"message","id":"2","parentId":"1","message":{"role":"assistant","content":[{"type":"text","text":"branch"}]}}
{"type":"message","id":"3","parentId":"2","message":{"role":"user","content":"left"}}
{"type":"message","id":"4","parentId":"2","message":{"role":"user","content":"right"}}
`),
	}

	truncated, err := ag.TruncateAtUUID(session, "3")
	if err != nil {
		t.Fatalf("TruncateAtUUID(tree) error = %v", err)
	}

	text := string(truncated.NativeData)
	if !strings.Contains(text, `"type":"session"`) {
		t.Fatalf("expected session header to be preserved, got: %s", text)
	}
	if !strings.Contains(text, `"id":"1"`) || !strings.Contains(text, `"id":"2"`) || !strings.Contains(text, `"id":"3"`) {
		t.Fatalf("expected root->target path to be preserved, got: %s", text)
	}
	if strings.Contains(text, `"id":"4"`) {
		t.Fatalf("expected sibling branch to be excluded, got: %s", text)
	}
}

func TestTruncateAtUUID_LargeLine(t *testing.T) {
	ag := &PiAgent{}
	huge := strings.Repeat("x", 11*1024*1024)
	session := &agent.AgentSession{
		SessionID:  "s-large",
		AgentName:  agent.AgentNamePi,
		SessionRef: "/tmp/x",
		NativeData: []byte(`{"type":"message","id":"1","message":{"role":"user","content":"` + huge + `"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}
`),
	}

	truncated, err := ag.TruncateAtUUID(session, "1")
	if err != nil {
		t.Fatalf("TruncateAtUUID() with large line error = %v", err)
	}
	if strings.Count(string(truncated.NativeData), "\n") != 1 {
		t.Fatalf("expected 1 line after truncation, got %d", strings.Count(string(truncated.NativeData), "\n"))
	}
}

func TestTruncateAtUUID_Errors(t *testing.T) {
	ag := &PiAgent{}

	if _, err := ag.TruncateAtUUID(nil, "1"); err == nil {
		t.Fatal("TruncateAtUUID(nil) expected error")
	}

	if _, err := ag.TruncateAtUUID(&agent.AgentSession{}, "1"); err == nil {
		t.Fatal("TruncateAtUUID(empty native data) expected error")
	}
}

func TestFindCheckpointUUID(t *testing.T) {
	ag := &PiAgent{}
	session := &agent.AgentSession{
		NativeData: []byte(`{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-1","name":"write","arguments":{"path":"a.go"}}]}}
{"type":"message","id":"2","message":{"role":"toolResult","toolName":"write","toolCallId":"tool-1","details":{"path":"a.go"}}}
`),
	}

	id, found := ag.FindCheckpointUUID(session, "tool-1")
	if !found {
		t.Fatal("FindCheckpointUUID() found = false, want true")
	}
	if id != "2" {
		t.Fatalf("FindCheckpointUUID() id = %q, want %q", id, "2")
	}

	if _, found := ag.FindCheckpointUUID(session, "missing"); found {
		t.Fatal("FindCheckpointUUID() found = true for missing tool id")
	}
}

func TestCalculateTokenUsage(t *testing.T) {
	ag := &PiAgent{}
	usage := ag.CalculateTokenUsage([]byte(`{"type":"message","id":"1","message":{"role":"assistant","usage":{"input_tokens":10,"output_tokens":4}}}
{"type":"message","id":"2","message":{"role":"assistant","tokens":{"inputTokens":3,"outputTokens":2}}}
`))

	if usage.APICallCount != 2 {
		t.Fatalf("APICallCount = %d, want 2", usage.APICallCount)
	}
	if usage.InputTokens != 13 {
		t.Fatalf("InputTokens = %d, want 13", usage.InputTokens)
	}
	if usage.OutputTokens != 6 {
		t.Fatalf("OutputTokens = %d, want 6", usage.OutputTokens)
	}
}

func TestGetTranscriptPosition(t *testing.T) {
	ag := &PiAgent{}

	if count, err := ag.GetTranscriptPosition(""); err != nil || count != 0 {
		t.Fatalf("GetTranscriptPosition(empty) = (%d, %v), want (0, nil)", count, err)
	}

	if count, err := ag.GetTranscriptPosition("/does/not/exist"); err != nil || count != 0 {
		t.Fatalf("GetTranscriptPosition(missing) = (%d, %v), want (0, nil)", count, err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"message","id":"1","message":{"role":"user","content":"one"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	count, err := ag.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("GetTranscriptPosition() = %d, want 2", count)
	}
}

func TestGetTranscriptPosition_NoTrailingNewline(t *testing.T) {
	ag := &PiAgent{}

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"message","id":"1","message":{"role":"user","content":"one"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	count, err := ag.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("GetTranscriptPosition() = %d, want 2", count)
	}
}

func TestExtractModifiedFilesFromOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","arguments":{"path":"a.go"}}]}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"toolCall","name":"edit","arguments":{"path":"b.go"}}]}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &PiAgent{}
	files, current, err := ag.ExtractModifiedFilesFromOffset(path, 1)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
	}
	if current != 2 {
		t.Fatalf("currentPosition = %d, want 2", current)
	}
	if len(files) != 1 || files[0] != "b.go" {
		t.Fatalf("files = %#v, want [b.go]", files)
	}
}

func TestChunkAndReassembleTranscript(t *testing.T) {
	ag := &PiAgent{}

	var b strings.Builder
	for i := range 20 {
		b.WriteString(`{"type":"message","id":"`)
		b.WriteRune(rune('a' + i))
		b.WriteString(`","message":{"role":"user","content":"line"}}`)
		b.WriteString("\n")
	}
	content := []byte(strings.TrimSuffix(b.String(), "\n"))

	chunks, err := ag.ChunkTranscript(content, 120)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}
	if string(reassembled) != string(content) {
		t.Fatalf("reassembled content mismatch")
	}
}

func TestSanitizePathForPi(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "unix absolute", in: "/repo/project", want: "--repo-project--"},
		{name: "windows absolute", in: `\repo\project`, want: "--repo-project--"},
		{name: "drive letter", in: `C:\repo\project`, want: "--C--repo-project--"},
		{name: "already relative", in: "repo/project", want: "--repo-project--"},
		{name: "empty", in: "", want: "----"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizePathForPi(tt.in)
			if got != tt.want {
				t.Fatalf("sanitizePathForPi(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
