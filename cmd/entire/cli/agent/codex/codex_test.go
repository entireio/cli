package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestNewCodexAgent(t *testing.T) {
	t.Parallel()

	ag := NewCodexAgent()
	if ag == nil {
		t.Fatal("NewCodexAgent() returned nil")
	}

	cx, ok := ag.(*CodexAgent)
	if !ok {
		t.Fatal("NewCodexAgent() didn't return *CodexAgent")
	}
	if cx == nil {
		t.Fatal("NewCodexAgent() returned nil agent")
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	if name := ag.Name(); name != agent.AgentNameCodex {
		t.Errorf("Name() = %q, want %q", name, agent.AgentNameCodex)
	}
}

func TestType(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	if agType := ag.Type(); agType != agent.AgentTypeCodex {
		t.Errorf("Type() = %q, want %q", agType, agent.AgentTypeCodex)
	}
}

func TestDescription(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	desc := ag.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestSupportsHooks(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	if !ag.SupportsHooks() {
		t.Error("SupportsHooks() = false, want true")
	}
}

func TestDetectPresence(t *testing.T) {
	t.Run("no .codex directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if present {
			t.Error("DetectPresence() = true, want false")
		}
	})

	t.Run("with .codex directory", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		if err := os.Mkdir(".codex", 0o755); err != nil {
			t.Fatalf("failed to create .codex: %v", err)
		}

		ag := &CodexAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})

	t.Run("with .codex/config.toml", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		if err := os.MkdirAll(".codex", 0o755); err != nil {
			t.Fatalf("failed to create .codex: %v", err)
		}
		if err := os.WriteFile(filepath.Join(".codex", "config.toml"), []byte(""), 0o600); err != nil {
			t.Fatalf("failed to create config.toml: %v", err)
		}

		ag := &CodexAgent{}
		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true")
		}
	})
}

func TestParseHookInput_AgentTurnComplete(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	input := `{"type":"agent-turn-complete","thread-id":"abc-123","turn-id":"turn-456","cwd":"/tmp","input-messages":["Fix the bug"],"last-assistant-message":"Done"}`

	result, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "abc-123")
	}
	if result.UserPrompt != "Fix the bug" {
		t.Errorf("UserPrompt = %q, want %q", result.UserPrompt, "Fix the bug")
	}
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	result := ag.ResolveSessionFile("/home/user/.codex/sessions", "abc-123")
	expected := "/home/user/.codex/sessions/abc-123.jsonl"
	if result != expected {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, expected)
	}
}

func TestProtectedDirs(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".codex" {
		t.Errorf("ProtectedDirs() = %v, want [.codex]", dirs)
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	cmd := ag.FormatResumeCommand("abc-123")
	expected := "codex resume abc-123"
	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

func TestTransformSessionID(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	id := ag.TransformSessionID("abc-123")
	if id != "abc-123" {
		t.Errorf("TransformSessionID() = %q, want %q", id, "abc-123")
	}
}

func TestGetHookNames(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	names := ag.GetHookNames()
	if len(names) != 1 || names[0] != HookNameAgentTurnComplete {
		t.Errorf("GetHookNames() = %v, want [%s]", names, HookNameAgentTurnComplete)
	}
}

func TestInstallHooks(t *testing.T) {
	t.Run("installs notify hook", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		count, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}
		if count != 1 {
			t.Errorf("InstallHooks() count = %d, want 1", count)
		}

		// Verify file contents
		data, err := os.ReadFile(filepath.Join(".codex", "config.toml"))
		if err != nil {
			t.Fatalf("failed to read config.toml: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, `notify = ["entire", "hooks", "codex", "agent-turn-complete"]`) {
			t.Errorf("config.toml missing notify line, got:\n%s", content)
		}
	})

	t.Run("idempotent install", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		// First install
		_, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Second install should be no-op
		count, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("second InstallHooks() error = %v", err)
		}
		if count != 0 {
			t.Errorf("second InstallHooks() count = %d, want 0", count)
		}
	})

	t.Run("local dev mode", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		count, err := ag.InstallHooks(true, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}
		if count != 1 {
			t.Errorf("InstallHooks() count = %d, want 1", count)
		}

		data, err := os.ReadFile(filepath.Join(".codex", "config.toml"))
		if err != nil {
			t.Fatalf("failed to read config.toml: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, `"go"`) || !strings.Contains(content, "entire") {
			t.Errorf("config.toml missing local dev notify line, got:\n%s", content)
		}
		// Verify env var was resolved at write-time (no ${...} templates)
		if strings.Contains(content, "${") {
			t.Errorf("config.toml contains unresolved env var template, got:\n%s", content)
		}
	})
}

func TestInstallHooks_ForceRemovesExistingNotify(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Pre-create config with a user notify line
	configPath := filepath.Join(".codex", "config.toml")
	if err := os.MkdirAll(".codex", 0o750); err != nil {
		t.Fatalf("failed to create .codex: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("notify = [\"my-custom-tool\"]\n"), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(false, true) // force=true
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Errorf("InstallHooks() count = %d, want 1", count)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	content := string(data)

	// Should have exactly one notify line (Entire's), not two
	if strings.Count(content, "notify = ") != 1 {
		t.Errorf("expected exactly 1 notify line, got:\n%s", content)
	}
	if !strings.Contains(content, `"entire"`) {
		t.Errorf("expected Entire notify line, got:\n%s", content)
	}
	if strings.Contains(content, "my-custom-tool") {
		t.Error("force install should have removed the user's custom notify line")
	}
}

func TestAreHooksInstalled(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		if ag.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = true, want false")
		}
	})

	t.Run("installed", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		_, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		if !ag.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = false, want true")
		}
	})
}

func TestUninstallHooks(t *testing.T) {
	t.Run("uninstalls notify hook", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		_, err := ag.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		if err := ag.UninstallHooks(); err != nil {
			t.Fatalf("UninstallHooks() error = %v", err)
		}

		if ag.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = true after uninstall")
		}
	})

	t.Run("no config file", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)

		ag := &CodexAgent{}
		if err := ag.UninstallHooks(); err != nil {
			t.Errorf("UninstallHooks() error = %v, want nil", err)
		}
	})
}

func TestGetSessionDir(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process-global state

	t.Run("with test override", func(t *testing.T) {
		t.Setenv("ENTIRE_TEST_CODEX_PROJECT_DIR", "/tmp/test-codex")
		ag := &CodexAgent{}
		dir, err := ag.GetSessionDir("/some/repo")
		if err != nil {
			t.Fatalf("GetSessionDir() error = %v", err)
		}
		if dir != "/tmp/test-codex" {
			t.Errorf("GetSessionDir() = %q, want %q", dir, "/tmp/test-codex")
		}
	})

	t.Run("with CODEX_HOME", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "/custom/codex")
		t.Setenv("ENTIRE_TEST_CODEX_PROJECT_DIR", "") // Clear test override
		ag := &CodexAgent{}
		dir, err := ag.GetSessionDir("/some/repo")
		if err != nil {
			t.Fatalf("GetSessionDir() error = %v", err)
		}
		expected := "/custom/codex/sessions"
		if dir != expected {
			t.Errorf("GetSessionDir() = %q, want %q", dir, expected)
		}
	})
}

func TestIsEntireNotifyLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "entire production hook",
			line: `notify = ["entire", "hooks", "codex", "agent-turn-complete"]`,
			want: true,
		},
		{
			name: "entire local dev hook with env var",
			line: `notify = ["go", "run", "${CODEX_PROJECT_DIR}/cmd/entire/main.go", "hooks", "codex", "agent-turn-complete"]`,
			want: true,
		},
		{
			name: "entire local dev hook with resolved path",
			line: `notify = ["go", "run", "/home/user/projects/entire/cmd/entire/main.go", "hooks", "codex", "agent-turn-complete"]`,
			want: true,
		},
		{
			name: "user custom hook",
			line: `notify = ["python3", "/path/to/my/script.py"]`,
			want: false,
		},
		{
			name: "empty notify",
			line: `notify = []`,
			want: false,
		},
		{
			name: "false positive - tool name containing entire substring",
			line: `notify = ["my-entire-tool"]`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isEntireNotifyLine(tt.line); got != tt.want {
				t.Errorf("isEntireNotifyLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestWriteSession_CreatesParentDir(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	sessionPath := filepath.Join(tempDir, "nested", "dir", "session.jsonl")

	ag := &CodexAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		SessionID:  "test-session",
		AgentName:  agent.AgentNameCodex,
		SessionRef: sessionPath,
		NativeData: []byte(`{"type":"session_meta"}`),
	})
	if err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		t.Error("WriteSession() did not create file")
	}
}

func TestGetTranscriptPosition_NoTrailingNewline(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	// Create a JSONL file with 3 lines, no trailing newline on last line
	filePath := filepath.Join(tempDir, "rollout.jsonl")
	content := `{"type":"session_meta"}
{"type":"response_item"}
{"type":"turn_context"}`
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	ag := &CodexAgent{}
	pos, err := ag.GetTranscriptPosition(filePath)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 3 {
		t.Errorf("GetTranscriptPosition() = %d, want 3 (should count final line without trailing newline)", pos)
	}
}

func TestResolveSessionFile_RecursiveSearch(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	// Create a nested date-based directory structure like Codex uses
	nestedDir := filepath.Join(tempDir, "2026", "02", "11")
	if err := os.MkdirAll(nestedDir, 0o750); err != nil {
		t.Fatalf("failed to create dirs: %v", err)
	}

	// Create rollout file in nested dir
	rolloutFile := filepath.Join(nestedDir, "rollout-abc123.jsonl")
	if err := os.WriteFile(rolloutFile, []byte(`{"type":"session_meta"}`), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	ag := &CodexAgent{}
	result := ag.ResolveSessionFile(tempDir, "abc123")
	if result != rolloutFile {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, rolloutFile)
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"item.completed","item":{"type":"file_change","file_path":"src/main.go"}}
{"type":"item.completed","item":{"type":"agent_message","text":"Done"}}
{"type":"item.completed","item":{"type":"file_change","path":"README.md"}}
`)

	files := ExtractModifiedFiles(data)
	if len(files) != 2 {
		t.Errorf("ExtractModifiedFiles() returned %d files, want 2", len(files))
	}
	if len(files) >= 1 && files[0] != "src/main.go" {
		t.Errorf("files[0] = %q, want %q", files[0], "src/main.go")
	}
	if len(files) >= 2 && files[1] != "README.md" {
		t.Errorf("files[1] = %q, want %q", files[1], "README.md")
	}
}

func TestExtractModifiedFiles_EventMsgPatchApplyBegin(t *testing.T) {
	t.Parallel()

	// Actual Codex rollout format: event_msg with patch_apply_begin payload
	// File paths are HashMap keys in the changes object
	data := []byte(`{"type":"event_msg","payload":{"type":"patch_apply_begin","changes":{"src/main.rs":{"status":"modified"},"src/lib.rs":{"status":"added"}}}}
{"type":"session_meta","payload":{"session_id":"abc"}}
{"type":"event_msg","payload":{"type":"patch_apply_begin","changes":{"README.md":{"status":"modified"}}}}
`)

	files := ExtractModifiedFiles(data)
	if len(files) != 3 {
		t.Fatalf("ExtractModifiedFiles() returned %d files, want 3, got: %v", len(files), files)
	}

	// Build a set for order-independent checking (HashMap iteration order is non-deterministic)
	fileSet := make(map[string]bool)
	for _, f := range files {
		fileSet[f] = true
	}

	for _, expected := range []string{"src/main.rs", "src/lib.rs", "README.md"} {
		if !fileSet[expected] {
			t.Errorf("expected file %q not found in result %v", expected, files)
		}
	}
}

func TestExtractModifiedFiles_MixedFormats(t *testing.T) {
	t.Parallel()

	// Mix of old-style item events and new event_msg format
	data := []byte(`{"type":"item.completed","item":{"type":"file_change","file_path":"old-format.go"}}
{"type":"event_msg","payload":{"type":"patch_apply_begin","changes":{"new-format.rs":{"status":"modified"}}}}
{"type":"item.completed","item":{"type":"function_call","name":"write_file","input":{"file_path":"func-call.py"}}}
`)

	files := ExtractModifiedFiles(data)
	if len(files) != 3 {
		t.Fatalf("ExtractModifiedFiles() returned %d files, want 3, got: %v", len(files), files)
	}

	fileSet := make(map[string]bool)
	for _, f := range files {
		fileSet[f] = true
	}

	for _, expected := range []string{"old-format.go", "new-format.rs", "func-call.py"} {
		if !fileSet[expected] {
			t.Errorf("expected file %q not found in result %v", expected, files)
		}
	}
}

func TestExtractModifiedFiles_LargeLine(t *testing.T) {
	t.Parallel()

	// Verify that lines > 64KB are handled correctly (bufio.Scanner would fail)
	longValue := strings.Repeat("a", 100_000)
	data := []byte(`{"type":"event_msg","payload":{"type":"patch_apply_begin","changes":{"large-file.txt":{"status":"modified"}}}}
{"type":"response_item","payload":{"text":"` + longValue + `"}}
{"type":"event_msg","payload":{"type":"patch_apply_begin","changes":{"after-large.txt":{"status":"added"}}}}
`)

	files := ExtractModifiedFiles(data)
	fileSet := make(map[string]bool)
	for _, f := range files {
		fileSet[f] = true
	}

	if !fileSet["large-file.txt"] {
		t.Error("expected large-file.txt in result")
	}
	if !fileSet["after-large.txt"] {
		t.Error("expected after-large.txt in result (should not be lost after large line)")
	}
}

func TestInstallHooks_NoOverwriteUserNotify(t *testing.T) {
	// Test that InstallHooks respects existing non-Entire notify lines
	// even when an Entire notify line was previously found and removed.
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Pre-create config with both an Entire line and a user line
	configPath := filepath.Join(".codex", "config.toml")
	if err := os.MkdirAll(".codex", 0o750); err != nil {
		t.Fatalf("failed to create .codex: %v", err)
	}
	content := "notify = [\"my-custom-tool\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	ag := &CodexAgent{}
	// Without force, should not overwrite user's custom notify line
	count, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 0 {
		t.Errorf("InstallHooks() count = %d, want 0 (should not overwrite user notify)", count)
	}

	// Verify user's line is still there
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("failed to read config: %v", readErr)
	}
	if !strings.Contains(string(data), "my-custom-tool") {
		t.Error("user's custom notify line was removed")
	}
}
