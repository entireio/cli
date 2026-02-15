package codexcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestCodexCLIAgent_Interface(t *testing.T) {
	t.Parallel()

	ag := NewCodexCLIAgent()

	if ag.Name() != agent.AgentNameCodex {
		t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameCodex)
	}
	if ag.Type() != agent.AgentTypeCodex {
		t.Errorf("Type() = %q, want %q", ag.Type(), agent.AgentTypeCodex)
	}
	if ag.Description() == "" {
		t.Error("Description() should not be empty")
	}
	if ag.SupportsHooks() {
		t.Error("SupportsHooks() should return false for Codex")
	}
	if ag.GetHookConfigPath() != "" {
		t.Errorf("GetHookConfigPath() = %q, want empty", ag.GetHookConfigPath())
	}
}

func TestCodexCLIAgent_ProtectedDirs(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 0 {
		t.Errorf("ProtectedDirs() = %v, want empty", dirs)
	}
}

func TestCodexCLIAgent_ParseHookInput_ReturnsError(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	_, err := ag.ParseHookInput(agent.HookStop, strings.NewReader("{}"))
	if err == nil {
		t.Error("ParseHookInput() should return error for Codex")
	}
}

func TestCodexCLIAgent_TransformSessionID(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	id := "0199a213-81c0-7800-8aa1-bbab2a035a53"
	if got := ag.TransformSessionID(id); got != id {
		t.Errorf("TransformSessionID(%q) = %q, want identity", id, got)
	}
}

func TestCodexCLIAgent_FormatResumeCommand(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	cmd := ag.FormatResumeCommand("session-123")
	if cmd != "codex exec resume session-123" {
		t.Errorf("FormatResumeCommand() = %q, unexpected", cmd)
	}
}

func TestCodexHome(t *testing.T) {
	t.Parallel()

	home := CodexHome()
	if home == "" {
		t.Error("CodexHome() should not be empty")
	}
	if !strings.HasSuffix(home, ".codex") {
		t.Errorf("CodexHome() = %q, should end with .codex", home)
	}
}

func TestCodexCLIAgent_Registration(t *testing.T) {
	t.Parallel()

	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatalf("agent.Get(%q) error = %v", agent.AgentNameCodex, err)
	}
	if ag.Name() != agent.AgentNameCodex {
		t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameCodex)
	}
}

func TestCodexCLIAgent_ReadSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := tmpDir + "/session.jsonl"
	if err := os.WriteFile(path, []byte(basicSessionJSONL), 0o600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	ag := &CodexCLIAgent{}
	input := &agent.HookInput{
		SessionID:  "test-session",
		SessionRef: path,
	}

	session, err := ag.ReadSession(input)
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if session.SessionID != "test-session" {
		t.Errorf("SessionID = %q, want %q", session.SessionID, "test-session")
	}
	if session.AgentName != agent.AgentNameCodex {
		t.Errorf("AgentName = %q, want %q", session.AgentName, agent.AgentNameCodex)
	}
	if len(session.ModifiedFiles) != 2 {
		t.Errorf("ModifiedFiles count = %d, want 2", len(session.ModifiedFiles))
	}
	if len(session.NativeData) == 0 {
		t.Error("NativeData should not be empty")
	}
}

func TestCodexCLIAgent_ReadSession_MissingRef(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	_, err := ag.ReadSession(&agent.HookInput{SessionID: "test"})
	if err == nil {
		t.Error("ReadSession() should return error when SessionRef is empty")
	}
}

func TestCodexCLIAgent_WriteSession(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	tmpDir := t.TempDir()
	path := tmpDir + "/test.jsonl"

	session := &agent.AgentSession{
		AgentName:  agent.AgentNameCodex,
		SessionRef: path,
		NativeData: []byte(`{"type":"thread.started","thread_id":"test"}`),
	}

	if err := ag.WriteSession(session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(data) != `{"type":"thread.started","thread_id":"test"}` {
		t.Errorf("written data = %q, unexpected", string(data))
	}
}

func TestCodexCLIAgent_WriteSession_WrongAgent(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		AgentName:  agent.AgentNameClaudeCode,
		SessionRef: "/tmp/test.jsonl",
		NativeData: []byte("data"),
	})
	if err == nil {
		t.Error("WriteSession() should return error for wrong agent")
	}
}

func TestCodexCLIAgent_GetSessionID(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	input := &agent.HookInput{SessionID: "thread-abc-123"}
	if got := ag.GetSessionID(input); got != "thread-abc-123" {
		t.Errorf("GetSessionID() = %q, want %q", got, "thread-abc-123")
	}
}

func TestCodexCLIAgent_ExtractAgentSessionID(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	got := ag.ExtractAgentSessionID("some-session-id")
	if got == "" {
		t.Error("ExtractAgentSessionID() should not return empty")
	}
}

func TestCodexCLIAgent_GetSessionDir(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	dir, err := ag.GetSessionDir("/some/repo")
	if err != nil {
		t.Fatalf("GetSessionDir() error = %v", err)
	}
	if !strings.Contains(dir, "sessions") {
		t.Errorf("GetSessionDir() = %q, should contain 'sessions'", dir)
	}
}

func TestCodexCLIAgent_ResolveSessionFile(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	got := ag.ResolveSessionFile("/sessions", "thread-123")
	want := filepath.Join("/sessions", "thread-123.jsonl")
	if got != want {
		t.Errorf("ResolveSessionFile() = %q, want %q", got, want)
	}
}

func TestCodexCLIAgent_ExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "session.jsonl")
	if err := os.WriteFile(path, []byte(basicSessionJSONL), 0o600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	ag := &CodexCLIAgent{}

	t.Run("from start", func(t *testing.T) {
		t.Parallel()
		files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
		if err != nil {
			t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
		}
		if len(files) != 2 {
			t.Errorf("files count = %d, want 2", len(files))
		}
		if pos != 8 {
			t.Errorf("position = %d, want 8", pos)
		}
	})

	t.Run("from offset past changes", func(t *testing.T) {
		t.Parallel()
		files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 8)
		if err != nil {
			t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
		}
		if len(files) != 0 {
			t.Errorf("files count = %d, want 0", len(files))
		}
		if pos != 8 {
			t.Errorf("position = %d, want 8", pos)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		files, pos, err := ag.ExtractModifiedFilesFromOffset("", 0)
		if err != nil {
			t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
		}
		if files != nil {
			t.Errorf("files = %v, want nil", files)
		}
		if pos != 0 {
			t.Errorf("position = %d, want 0", pos)
		}
	})
}

func TestCodexCLIAgent_ChunkTranscript(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	data := []byte(basicSessionJSONL)

	chunks, err := ag.ChunkTranscript(data, 500)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) == 0 {
		t.Error("ChunkTranscript() returned no chunks")
	}

	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}
	if len(reassembled) == 0 {
		t.Error("ReassembleTranscript() returned empty")
	}
}

func TestCodexCLIAgent_DetectPresence(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	// DetectPresence checks PATH for "codex" binary — result depends on environment
	// but it should not return an error regardless
	_, err := ag.DetectPresence()
	if err != nil {
		t.Errorf("DetectPresence() error = %v", err)
	}
}

func TestCodexCLIAgent_DetectPresence_NotInPath(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process-global state
	t.Setenv("PATH", t.TempDir())

	ag := &CodexCLIAgent{}
	found, err := ag.DetectPresence()
	if err != nil {
		t.Errorf("DetectPresence() error = %v", err)
	}
	if found {
		t.Error("DetectPresence() = true, want false when codex not in PATH")
	}
}

func TestCodexHome_WithEnvVar(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process-global state
	t.Setenv("CODEX_HOME", "/custom/codex/home")

	home := CodexHome()
	if home != "/custom/codex/home" {
		t.Errorf("CodexHome() = %q, want %q", home, "/custom/codex/home")
	}
}

func TestCodexHome_UserHomeDirError(t *testing.T) {
	// Cannot use t.Parallel() — modifies package-level state
	t.Setenv("CODEX_HOME", "")

	orig := userHomeDir
	userHomeDir = func() (string, error) {
		return "", errors.New("no home dir")
	}
	t.Cleanup(func() { userHomeDir = orig })

	home := CodexHome()
	want := filepath.Join(".", ".codex")
	if home != want {
		t.Errorf("CodexHome() = %q, want %q", home, want)
	}
}

func TestCodexCLIAgent_ReadSession_FileNotFound(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	_, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "test",
		SessionRef: "/nonexistent/path/session.jsonl",
	})
	if err == nil {
		t.Error("ReadSession() should return error for nonexistent file")
	}
}

func TestCodexCLIAgent_ReadSession_ParseError(t *testing.T) {
	// Cannot use t.Parallel() — modifies package-level state
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"thread.started"}`+"\n"), 0o600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	orig := parseEventStreamFn
	parseEventStreamFn = func(_ []byte) (*ParsedSession, error) {
		return nil, errors.New("injected parse error")
	}
	t.Cleanup(func() { parseEventStreamFn = orig })

	ag := &CodexCLIAgent{}
	_, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "test",
		SessionRef: path,
	})
	if err == nil {
		t.Error("ReadSession() should return error when parsing fails")
	}
}

func TestCodexCLIAgent_WriteSession_Nil(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	if err := ag.WriteSession(nil); err == nil {
		t.Error("WriteSession(nil) should return error")
	}
}

func TestCodexCLIAgent_WriteSession_EmptyRef(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		AgentName:  agent.AgentNameCodex,
		NativeData: []byte("data"),
	})
	if err == nil {
		t.Error("WriteSession() should return error when SessionRef is empty")
	}
}

func TestCodexCLIAgent_WriteSession_EmptyNativeData(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		AgentName:  agent.AgentNameCodex,
		SessionRef: "/tmp/test.jsonl",
	})
	if err == nil {
		t.Error("WriteSession() should return error when NativeData is empty")
	}
}

func TestCodexCLIAgent_WriteSession_InvalidPath(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		AgentName:  agent.AgentNameCodex,
		SessionRef: "/nonexistent/deeply/nested/dir/session.jsonl",
		NativeData: []byte("data"),
	})
	if err == nil {
		t.Error("WriteSession() should return error for invalid path")
	}
}

func TestCodexCLIAgent_GetTranscriptPosition_Method(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "session.jsonl")
	if err := os.WriteFile(path, []byte(basicSessionJSONL), 0o600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	ag := &CodexCLIAgent{}
	pos, err := ag.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 8 {
		t.Errorf("GetTranscriptPosition() = %d, want 8", pos)
	}
}

func TestCodexCLIAgent_ExtractModifiedFilesFromOffset_FileOpenError(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	_, _, err := ag.ExtractModifiedFilesFromOffset("/nonexistent/session.jsonl", 0)
	if err == nil {
		t.Error("ExtractModifiedFilesFromOffset() should return error for nonexistent file")
	}
}

func TestCodexCLIAgent_ExtractModifiedFilesFromOffset_MalformedData(t *testing.T) {
	t.Parallel()

	// Lines cover: event unmarshal error, envelope unmarshal error, file_change unmarshal error
	data := `{"type":"thread.started","thread_id":"test"}
not valid json at all
{"type":"item.completed","item":"not an object"}
{"type":"item.completed","item":{"type":"file_change","changes":"not an array"}}
{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"good.go","kind":"add"}]}}
{"type":"turn.completed"}
`

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "malformed.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	ag := &CodexCLIAgent{}
	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
	}
	if len(files) != 1 || files[0] != "good.go" {
		t.Errorf("files = %v, want [good.go]", files)
	}
	if pos != 6 {
		t.Errorf("position = %d, want 6", pos)
	}
}

func TestCodexCLIAgent_ChunkTranscript_LineTooLarge(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	// A line longer than maxSize triggers an error from ChunkJSONL
	data := []byte(`{"type":"thread.started","thread_id":"test-chunk-error"}`)
	_, err := ag.ChunkTranscript(data, 5)
	if err == nil {
		t.Error("ChunkTranscript() should return error when a line exceeds maxSize")
	}
}

// errorReader is a reader that returns an error after delivering some data.
type errorReader struct {
	data  []byte
	pos   int
	errAt int
	err   error
}

func (r *errorReader) Read(p []byte) (int, error) {
	if r.pos >= r.errAt {
		return 0, r.err
	}
	end := r.pos + len(p)
	if end > len(r.data) {
		end = len(r.data)
	}
	if end > r.errAt {
		end = r.errAt
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	if r.pos >= r.errAt {
		return n, r.err
	}
	return n, nil
}

func TestScanModifiedFiles_ScannerError(t *testing.T) {
	t.Parallel()

	data := `{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"a.go","kind":"add"}]}}` + "\n"
	r := &errorReader{
		data:  []byte(data),
		errAt: len(data) / 2, // error partway through
		err:   errors.New("injected IO error"),
	}

	_, _, err := scanModifiedFiles(r, 0)
	if err == nil {
		t.Error("scanModifiedFiles() should return error on reader failure")
	}
}
