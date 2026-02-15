//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
)

// TestAgentDetection verifies agent detection and default behavior.
// Not parallel - contains subtests that use os.Chdir which is process-global.
func TestAgentDetection(t *testing.T) {

	t.Run("defaults to claude-code when nothing configured", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		// No .claude directory, no .entire settings
		ag, err := agent.Get(agent.DefaultAgentName)
		if err != nil {
			t.Fatalf("Get(default) error = %v", err)
		}
		if ag.Name() != "claude-code" {
			t.Errorf("default agent = %q, want %q", ag.Name(), "claude-code")
		}
	})

	t.Run("claude-code detects presence when .claude exists", func(t *testing.T) {
		// Not parallel - uses os.Chdir which is process-global
		env := NewTestEnv(t)
		env.InitRepo()

		// Create .claude/settings.json
		claudeDir := filepath.Join(env.RepoDir, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatalf("failed to create .claude dir: %v", err)
		}
		settingsPath := filepath.Join(claudeDir, claudecode.ClaudeSettingsFileName)
		if err := os.WriteFile(settingsPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
			t.Fatalf("failed to write settings.json: %v", err)
		}

		// Change to repo dir for detection
		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, err := agent.Get("claude-code")
		if err != nil {
			t.Fatalf("Get(claude-code) error = %v", err)
		}

		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true when .claude exists")
		}
	})

	t.Run("agent registry lists claude-code", func(t *testing.T) {
		t.Parallel()

		agents := agent.List()
		found := false
		for _, name := range agents {
			if name == "claude-code" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agent.List() = %v, want to contain 'claude-code'", agents)
		}
	})
}

// TestAgentHookInstallation verifies hook installation via agent interface.
// Note: These tests cannot run in parallel because they use os.Chdir which affects the entire process.
func TestAgentHookInstallation(t *testing.T) {
	// Not parallel - tests use os.Chdir which is process-global

	t.Run("installs all required hooks", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		// Change to repo dir
		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, err := agent.Get("claude-code")
		if err != nil {
			t.Fatalf("Get(claude-code) error = %v", err)
		}

		hookAgent, ok := ag.(agent.HookSupport)
		if !ok {
			t.Fatal("claude-code agent does not implement HookSupport")
		}

		count, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		// Should install 7 hooks: SessionStart, SessionEnd, Stop, UserPromptSubmit, PreToolUse[Task], PostToolUse[Task], PostToolUse[TodoWrite]
		if count != 7 {
			t.Errorf("InstallHooks() count = %d, want 7", count)
		}

		// Verify hooks are installed
		if !hookAgent.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = false after InstallHooks()")
		}

		// Verify settings.json was created
		settingsPath := filepath.Join(env.RepoDir, ".claude", claudecode.ClaudeSettingsFileName)
		if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
			t.Error("settings.json was not created")
		}

		// Verify permissions.deny contains metadata deny rule
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("failed to read settings.json: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, "Read(./.entire/metadata/**)") {
			t.Error("settings.json should contain permissions.deny rule for .entire/metadata/**")
		}
	})

	t.Run("idempotent - second install returns 0", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("claude-code")
		hookAgent := ag.(agent.HookSupport)

		// First install
		_, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Second install should be idempotent
		count, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("second InstallHooks() error = %v", err)
		}
		if count != 0 {
			t.Errorf("second InstallHooks() count = %d, want 0 (idempotent)", count)
		}
	})

	t.Run("localDev mode uses go run", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("claude-code")
		hookAgent := ag.(agent.HookSupport)

		_, err := hookAgent.InstallHooks(true, false) // localDev = true
		if err != nil {
			t.Fatalf("InstallHooks(localDev=true) error = %v", err)
		}

		// Read settings and verify commands use "go run"
		settingsPath := filepath.Join(env.RepoDir, ".claude", claudecode.ClaudeSettingsFileName)
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("failed to read settings.json: %v", err)
		}

		content := string(data)
		if !strings.Contains(content, "go run") {
			t.Error("localDev hooks should use 'go run', but settings.json doesn't contain it")
		}
	})
}

// TestAgentSessionOperations verifies ReadSession/WriteSession via agent interface.
func TestAgentSessionOperations(t *testing.T) {
	t.Parallel()

	t.Run("ReadSession parses transcript and computes ModifiedFiles", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		// Create a transcript file
		transcriptPath := filepath.Join(env.RepoDir, "test-transcript.jsonl")
		transcriptContent := `{"type":"user","uuid":"u1","message":{"content":"Fix the bug"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"I'll fix it"},{"type":"tool_use","name":"Write","input":{"file_path":"main.go"}}]}}
{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"a1"}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"util.go"}}]}}
`
		if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get("claude-code")
		session, err := ag.ReadSession(&agent.HookInput{
			SessionID:  "test-session",
			SessionRef: transcriptPath,
		})
		if err != nil {
			t.Fatalf("ReadSession() error = %v", err)
		}

		// Verify session metadata
		if session.SessionID != "test-session" {
			t.Errorf("SessionID = %q, want %q", session.SessionID, "test-session")
		}
		if session.AgentName != "claude-code" {
			t.Errorf("AgentName = %q, want %q", session.AgentName, "claude-code")
		}

		// Verify NativeData is populated
		if len(session.NativeData) == 0 {
			t.Error("NativeData is empty, want transcript content")
		}

		// Verify ModifiedFiles computed
		if len(session.ModifiedFiles) != 2 {
			t.Errorf("ModifiedFiles = %v, want 2 files (main.go, util.go)", session.ModifiedFiles)
		}
	})

	t.Run("WriteSession writes NativeData to file", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		ag, _ := agent.Get("claude-code")

		// First read a session
		srcPath := filepath.Join(env.RepoDir, "src.jsonl")
		srcContent := `{"type":"user","uuid":"u1","message":{"content":"hello"}}
`
		if err := os.WriteFile(srcPath, []byte(srcContent), 0o644); err != nil {
			t.Fatalf("failed to write source: %v", err)
		}

		session, _ := ag.ReadSession(&agent.HookInput{
			SessionID:  "test",
			SessionRef: srcPath,
		})

		// Write to a new location
		dstPath := filepath.Join(env.RepoDir, "dst.jsonl")
		session.SessionRef = dstPath

		if err := ag.WriteSession(session); err != nil {
			t.Fatalf("WriteSession() error = %v", err)
		}

		// Verify file was written
		data, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatalf("failed to read destination: %v", err)
		}
		if string(data) != srcContent {
			t.Errorf("written content = %q, want %q", string(data), srcContent)
		}
	})

	t.Run("WriteSession rejects wrong agent", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("claude-code")

		session := &agent.AgentSession{
			SessionID:  "test",
			AgentName:  "other-agent", // Wrong agent
			SessionRef: "/tmp/test.jsonl",
			NativeData: []byte("data"),
		}

		err := ag.WriteSession(session)
		if err == nil {
			t.Error("WriteSession() should reject session from different agent")
		}
	})
}

// TestClaudeCodeHelperMethods verifies Claude-specific helper methods.
func TestClaudeCodeHelperMethods(t *testing.T) {
	t.Parallel()

	t.Run("GetLastUserPrompt extracts last user message", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)

		transcriptPath := filepath.Join(env.RepoDir, "transcript.jsonl")
		content := `{"type":"user","uuid":"u1","message":{"content":"first prompt"}}
{"type":"assistant","uuid":"a1","message":{"content":[]}}
{"type":"user","uuid":"u2","message":{"content":"second prompt"}}
{"type":"assistant","uuid":"a2","message":{"content":[]}}
`
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get("claude-code")
		ccAgent := ag.(*claudecode.ClaudeCodeAgent)

		session, _ := ag.ReadSession(&agent.HookInput{
			SessionID:  "test",
			SessionRef: transcriptPath,
		})

		prompt := ccAgent.GetLastUserPrompt(session)
		if prompt != "second prompt" {
			t.Errorf("GetLastUserPrompt() = %q, want %q", prompt, "second prompt")
		}
	})

	t.Run("TruncateAtUUID truncates transcript", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)

		transcriptPath := filepath.Join(env.RepoDir, "transcript.jsonl")
		content := `{"type":"user","uuid":"u1","message":{"content":"first"}}
{"type":"assistant","uuid":"a1","message":{"content":[]}}
{"type":"user","uuid":"u2","message":{"content":"second"}}
{"type":"assistant","uuid":"a2","message":{"content":[]}}
`
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get("claude-code")
		ccAgent := ag.(*claudecode.ClaudeCodeAgent)

		session, _ := ag.ReadSession(&agent.HookInput{
			SessionID:  "test",
			SessionRef: transcriptPath,
		})

		truncated, err := ccAgent.TruncateAtUUID(session, "a1")
		if err != nil {
			t.Fatalf("TruncateAtUUID() error = %v", err)
		}

		// Parse the truncated native data to verify
		lines, _ := claudecode.ParseTranscript(truncated.NativeData)
		if len(lines) != 2 {
			t.Errorf("truncated transcript has %d lines, want 2", len(lines))
		}
		if lines[len(lines)-1].UUID != "a1" {
			t.Errorf("last line UUID = %q, want %q", lines[len(lines)-1].UUID, "a1")
		}
	})

	t.Run("FindCheckpointUUID finds tool result", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)

		transcriptPath := filepath.Join(env.RepoDir, "transcript.jsonl")
		content := `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"tool-123"}]}}
{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"tool-123"}]}}
`
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get("claude-code")
		ccAgent := ag.(*claudecode.ClaudeCodeAgent)

		session, _ := ag.ReadSession(&agent.HookInput{
			SessionID:  "test",
			SessionRef: transcriptPath,
		})

		uuid, found := ccAgent.FindCheckpointUUID(session, "tool-123")
		if !found {
			t.Error("FindCheckpointUUID() found = false, want true")
		}
		if uuid != "u1" {
			t.Errorf("FindCheckpointUUID() uuid = %q, want %q", uuid, "u1")
		}
	})

}

// TestGeminiCLIAgentDetection verifies Gemini CLI agent detection.
// Not parallel - contains subtests that use os.Chdir which is process-global.
func TestGeminiCLIAgentDetection(t *testing.T) {

	t.Run("gemini agent is registered", func(t *testing.T) {
		t.Parallel()

		agents := agent.List()
		found := false
		for _, name := range agents {
			if name == "gemini" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agent.List() = %v, want to contain 'gemini'", agents)
		}
	})

	t.Run("gemini detects presence when .gemini exists", func(t *testing.T) {
		// Not parallel - uses os.Chdir which is process-global
		env := NewTestEnv(t)
		env.InitRepo()

		// Create .gemini/settings.json
		geminiDir := filepath.Join(env.RepoDir, ".gemini")
		if err := os.MkdirAll(geminiDir, 0o755); err != nil {
			t.Fatalf("failed to create .gemini dir: %v", err)
		}
		settingsPath := filepath.Join(geminiDir, geminicli.GeminiSettingsFileName)
		if err := os.WriteFile(settingsPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
			t.Fatalf("failed to write settings.json: %v", err)
		}

		// Change to repo dir for detection
		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, err := agent.Get("gemini")
		if err != nil {
			t.Fatalf("Get(gemini) error = %v", err)
		}

		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true when .gemini exists")
		}
	})
}

// TestGeminiCLIHookInstallation verifies hook installation via Gemini CLI agent interface.
// Note: These tests cannot run in parallel because they use os.Chdir which affects the entire process.
func TestGeminiCLIHookInstallation(t *testing.T) {
	// Not parallel - tests use os.Chdir which is process-global

	t.Run("installs all required hooks", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		// Change to repo dir
		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, err := agent.Get("gemini")
		if err != nil {
			t.Fatalf("Get(gemini) error = %v", err)
		}

		hookAgent, ok := ag.(agent.HookSupport)
		if !ok {
			t.Fatal("gemini agent does not implement HookSupport")
		}

		count, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		// Should install 12 hooks: SessionStart, SessionEnd (exit+logout), BeforeAgent, AfterAgent,
		// BeforeModel, AfterModel, BeforeToolSelection, BeforeTool, AfterTool, PreCompress, Notification
		if count != 12 {
			t.Errorf("InstallHooks() count = %d, want 12", count)
		}

		// Verify hooks are installed
		if !hookAgent.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = false after InstallHooks()")
		}

		// Verify settings.json was created
		settingsPath := filepath.Join(env.RepoDir, ".gemini", geminicli.GeminiSettingsFileName)
		if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
			t.Error("settings.json was not created")
		}

		// Verify hooks structure in settings.json
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("failed to read settings.json: %v", err)
		}
		content := string(data)

		// Verify all hook types are present
		if !strings.Contains(content, "SessionStart") {
			t.Error("settings.json should contain SessionStart hook")
		}
		if !strings.Contains(content, "SessionEnd") {
			t.Error("settings.json should contain SessionEnd hook")
		}
		if !strings.Contains(content, "BeforeAgent") {
			t.Error("settings.json should contain BeforeAgent hook")
		}
		if !strings.Contains(content, "AfterAgent") {
			t.Error("settings.json should contain AfterAgent hook")
		}
		if !strings.Contains(content, "BeforeModel") {
			t.Error("settings.json should contain BeforeModel hook")
		}
		if !strings.Contains(content, "AfterModel") {
			t.Error("settings.json should contain AfterModel hook")
		}
		if !strings.Contains(content, "BeforeToolSelection") {
			t.Error("settings.json should contain BeforeToolSelection hook")
		}
		if !strings.Contains(content, "BeforeTool") {
			t.Error("settings.json should contain BeforeTool hook")
		}
		if !strings.Contains(content, "AfterTool") {
			t.Error("settings.json should contain AfterTool hook")
		}
		if !strings.Contains(content, "PreCompress") {
			t.Error("settings.json should contain PreCompress hook")
		}
		if !strings.Contains(content, "Notification") {
			t.Error("settings.json should contain Notification hook")
		}

		// Verify hooksConfig is set
		if !strings.Contains(content, "hooksConfig") {
			t.Error("settings.json should contain hooksConfig.enabled")
		}
	})

	t.Run("idempotent - second install returns 0", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("gemini")
		hookAgent := ag.(agent.HookSupport)

		// First install
		_, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Second install should be idempotent
		count, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("second InstallHooks() error = %v", err)
		}
		if count != 0 {
			t.Errorf("second InstallHooks() count = %d, want 0 (idempotent)", count)
		}
	})

	t.Run("localDev mode uses go run", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("gemini")
		hookAgent := ag.(agent.HookSupport)

		_, err := hookAgent.InstallHooks(true, false) // localDev = true
		if err != nil {
			t.Fatalf("InstallHooks(localDev=true) error = %v", err)
		}

		// Read settings and verify commands use "go run"
		settingsPath := filepath.Join(env.RepoDir, ".gemini", geminicli.GeminiSettingsFileName)
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("failed to read settings.json: %v", err)
		}

		content := string(data)
		if !strings.Contains(content, "go run") {
			t.Error("localDev hooks should use 'go run', but settings.json doesn't contain it")
		}
		if !strings.Contains(content, "${GEMINI_PROJECT_DIR}") {
			t.Error("localDev hooks should use '${GEMINI_PROJECT_DIR}', but settings.json doesn't contain it")
		}
	})

	t.Run("production mode uses entire binary", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("gemini")
		hookAgent := ag.(agent.HookSupport)

		_, err := hookAgent.InstallHooks(false, false) // localDev = false
		if err != nil {
			t.Fatalf("InstallHooks(localDev=false) error = %v", err)
		}

		// Read settings and verify commands use "entire" binary
		settingsPath := filepath.Join(env.RepoDir, ".gemini", geminicli.GeminiSettingsFileName)
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("failed to read settings.json: %v", err)
		}

		content := string(data)
		if !strings.Contains(content, "entire hooks gemini") {
			t.Error("production hooks should use 'entire hooks gemini', but settings.json doesn't contain it")
		}
	})

	t.Run("force flag reinstalls hooks", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("gemini")
		hookAgent := ag.(agent.HookSupport)

		// First install
		_, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Force reinstall should return count > 0
		count, err := hookAgent.InstallHooks(false, true) // force = true
		if err != nil {
			t.Fatalf("force InstallHooks() error = %v", err)
		}
		if count != 12 {
			t.Errorf("force InstallHooks() count = %d, want 12", count)
		}
	})
}

// TestGeminiCLISessionOperations verifies ReadSession/WriteSession via Gemini agent interface.
func TestGeminiCLISessionOperations(t *testing.T) {
	t.Parallel()

	t.Run("ReadSession parses transcript and computes ModifiedFiles", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		// Create a Gemini transcript file (JSON format)
		// Gemini uses "type" field with values "user" or "gemini", and "toolCalls" array with "args"
		transcriptPath := filepath.Join(env.RepoDir, "test-transcript.json")
		transcriptContent := `{
  "messages": [
    {"type": "user", "content": "Fix the bug"},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "main.go"}}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "edit_file", "args": {"file_path": "util.go"}}]}
  ]
}`
		if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get("gemini")
		session, err := ag.ReadSession(&agent.HookInput{
			SessionID:  "test-session",
			SessionRef: transcriptPath,
		})
		if err != nil {
			t.Fatalf("ReadSession() error = %v", err)
		}

		// Verify session metadata
		if session.SessionID != "test-session" {
			t.Errorf("SessionID = %q, want %q", session.SessionID, "test-session")
		}
		if session.AgentName != "gemini" {
			t.Errorf("AgentName = %q, want %q", session.AgentName, "gemini")
		}

		// Verify NativeData is populated
		if len(session.NativeData) == 0 {
			t.Error("NativeData is empty, want transcript content")
		}

		// Verify ModifiedFiles computed
		if len(session.ModifiedFiles) != 2 {
			t.Errorf("ModifiedFiles = %v, want 2 files (main.go, util.go)", session.ModifiedFiles)
		}
	})

	t.Run("WriteSession writes NativeData to file", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		ag, _ := agent.Get("gemini")

		// First read a session
		srcPath := filepath.Join(env.RepoDir, "src.json")
		srcContent := `{"messages": [{"role": "user", "content": "hello"}]}`
		if err := os.WriteFile(srcPath, []byte(srcContent), 0o644); err != nil {
			t.Fatalf("failed to write source: %v", err)
		}

		session, _ := ag.ReadSession(&agent.HookInput{
			SessionID:  "test",
			SessionRef: srcPath,
		})

		// Write to a new location
		dstPath := filepath.Join(env.RepoDir, "dst.json")
		session.SessionRef = dstPath

		if err := ag.WriteSession(session); err != nil {
			t.Fatalf("WriteSession() error = %v", err)
		}

		// Verify file was written
		data, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatalf("failed to read destination: %v", err)
		}
		if string(data) != srcContent {
			t.Errorf("written content = %q, want %q", string(data), srcContent)
		}
	})

	t.Run("WriteSession rejects wrong agent", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("gemini")

		session := &agent.AgentSession{
			SessionID:  "test",
			AgentName:  "other-agent", // Wrong agent
			SessionRef: "/tmp/test.json",
			NativeData: []byte("data"),
		}

		err := ag.WriteSession(session)
		if err == nil {
			t.Error("WriteSession() should reject session from different agent")
		}
	})
}

// TestGeminiCLIHelperMethods verifies Gemini-specific helper methods.
func TestGeminiCLIHelperMethods(t *testing.T) {
	t.Parallel()

	t.Run("FormatResumeCommand returns gemini --resume", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("gemini")
		cmd := ag.FormatResumeCommand("abc123")

		if cmd != "gemini --resume abc123" {
			t.Errorf("FormatResumeCommand() = %q, want %q", cmd, "gemini --resume abc123")
		}
	})

	t.Run("GetHookConfigPath returns .gemini/settings.json", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("gemini")
		path := ag.GetHookConfigPath()

		if path != ".gemini/settings.json" {
			t.Errorf("GetHookConfigPath() = %q, want %q", path, ".gemini/settings.json")
		}
	})
}

// ============================================================
// OpenCode Agent Tests
// ============================================================

// TestOpenCodeAgentDetection verifies OpenCode agent detection and registration.
func TestOpenCodeAgentDetection(t *testing.T) {

	t.Run("opencode agent is registered", func(t *testing.T) {
		t.Parallel()

		agents := agent.List()
		found := false
		for _, name := range agents {
			if name == "opencode" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agent.List() = %v, want to contain 'opencode'", agents)
		}
	})

	t.Run("opencode detects presence when .opencode exists", func(t *testing.T) {
		// Not parallel - uses os.Chdir which is process-global
		env := NewTestEnv(t)
		env.InitRepo()

		// Create .opencode directory
		opencodeDir := filepath.Join(env.RepoDir, ".opencode")
		if err := os.MkdirAll(opencodeDir, 0o755); err != nil {
			t.Fatalf("failed to create .opencode dir: %v", err)
		}

		// Change to repo dir for detection
		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, err := agent.Get("opencode")
		if err != nil {
			t.Fatalf("Get(opencode) error = %v", err)
		}

		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if !present {
			t.Error("DetectPresence() = false, want true when .opencode exists")
		}
	})

	t.Run("opencode does not detect presence when .opencode absent", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, err := agent.Get("opencode")
		if err != nil {
			t.Fatalf("Get(opencode) error = %v", err)
		}

		present, err := ag.DetectPresence()
		if err != nil {
			t.Fatalf("DetectPresence() error = %v", err)
		}
		if present {
			t.Error("DetectPresence() = true, want false when .opencode doesn't exist")
		}
	})
}

// TestOpenCodeHookInstallation verifies hook installation via OpenCode agent interface.
func TestOpenCodeHookInstallation(t *testing.T) {
	// Not parallel - tests use os.Chdir

	t.Run("installs plugin file", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, err := agent.Get("opencode")
		if err != nil {
			t.Fatalf("Get(opencode) error = %v", err)
		}

		hookAgent, ok := ag.(agent.HookSupport)
		if !ok {
			t.Fatal("opencode agent does not implement HookSupport")
		}

		count, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		// Should install 1 hook (the plugin file)
		if count != 1 {
			t.Errorf("InstallHooks() count = %d, want 1", count)
		}

		// Verify plugin file was created
		pluginPath := filepath.Join(env.RepoDir, ".opencode", "plugins", "entire.ts")
		if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
			t.Error("plugin file was not created at .opencode/plugins/entire.ts")
		}

		// Verify hooks are installed
		if !hookAgent.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = false after InstallHooks()")
		}
	})

	t.Run("idempotent - second install returns 0", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("opencode")
		hookAgent := ag.(agent.HookSupport)

		// First install
		_, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Second install should be idempotent
		count, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("second InstallHooks() error = %v", err)
		}
		if count != 0 {
			t.Errorf("second InstallHooks() count = %d, want 0 (idempotent)", count)
		}
	})

	t.Run("force flag reinstalls plugin", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("opencode")
		hookAgent := ag.(agent.HookSupport)

		// First install
		_, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("first InstallHooks() error = %v", err)
		}

		// Force reinstall should return count > 0
		count, err := hookAgent.InstallHooks(false, true) // force = true
		if err != nil {
			t.Fatalf("force InstallHooks() error = %v", err)
		}
		if count != 1 {
			t.Errorf("force InstallHooks() count = %d, want 1", count)
		}
	})

	t.Run("uninstall removes plugin file", func(t *testing.T) {
		// Not parallel - uses os.Chdir
		env := NewTestEnv(t)
		env.InitRepo()

		oldWd, _ := os.Getwd()
		if err := os.Chdir(env.RepoDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(oldWd) }()

		ag, _ := agent.Get("opencode")
		hookAgent := ag.(agent.HookSupport)

		// Install
		_, err := hookAgent.InstallHooks(false, false)
		if err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}

		// Uninstall
		if err := hookAgent.UninstallHooks(); err != nil {
			t.Fatalf("UninstallHooks() error = %v", err)
		}

		// Verify plugin file is removed
		pluginPath := filepath.Join(env.RepoDir, ".opencode", "plugins", "entire.ts")
		if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
			t.Error("plugin file should be removed after UninstallHooks()")
		}

		// Verify hooks are no longer installed
		if hookAgent.AreHooksInstalled() {
			t.Error("AreHooksInstalled() = true after UninstallHooks()")
		}
	})
}

// TestOpenCodeSessionOperations verifies ReadSession/WriteSession via OpenCode agent interface.
func TestOpenCodeSessionOperations(t *testing.T) {
	t.Parallel()

	t.Run("ReadSession parses transcript and computes ModifiedFiles", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		// Create an OpenCode JSONL transcript
		transcriptPath := filepath.Join(env.RepoDir, "test-transcript.jsonl")
		session := env.NewOpencodeSession()
		content := session.CreateOpencodeTranscript("Fix the bug", []FileChange{
			{Path: "main.go", Content: "package main"},
			{Path: "util.go", Content: "package util"},
		})

		ag, _ := agent.Get("opencode")
		agentSession, err := ag.ReadSession(&agent.HookInput{
			SessionID:  "test-session",
			SessionRef: content,
		})
		if err != nil {
			t.Fatalf("ReadSession() error = %v", err)
		}

		// Verify session metadata
		if agentSession.SessionID != "test-session" {
			t.Errorf("SessionID = %q, want %q", agentSession.SessionID, "test-session")
		}
		if agentSession.AgentName != "opencode" {
			t.Errorf("AgentName = %q, want %q", agentSession.AgentName, "opencode")
		}

		// Verify NativeData is populated
		if len(agentSession.NativeData) == 0 {
			t.Error("NativeData is empty, want transcript content")
		}

		// Verify ModifiedFiles computed
		if len(agentSession.ModifiedFiles) != 2 {
			t.Errorf("ModifiedFiles = %v, want 2 files (main.go, util.go)", agentSession.ModifiedFiles)
		}

		_ = transcriptPath // keep for clarity
	})

	t.Run("WriteSession writes NativeData to file", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		ag, _ := agent.Get("opencode")

		// Create source transcript
		srcPath := filepath.Join(env.RepoDir, "src.jsonl")
		srcContent := `{"info":{"id":"msg-1","sessionID":"s1","role":"user","time":{"created":1,"completed":2}},"parts":[{"type":"text","text":"hello"}]}
`
		if err := os.WriteFile(srcPath, []byte(srcContent), 0o644); err != nil {
			t.Fatalf("failed to write source: %v", err)
		}

		session, _ := ag.ReadSession(&agent.HookInput{
			SessionID:  "test",
			SessionRef: srcPath,
		})

		// Write to a new location
		dstPath := filepath.Join(env.RepoDir, "dst.jsonl")
		session.SessionRef = dstPath

		if err := ag.WriteSession(session); err != nil {
			t.Fatalf("WriteSession() error = %v", err)
		}

		// Verify file was written
		data, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatalf("failed to read destination: %v", err)
		}
		if string(data) != srcContent {
			t.Errorf("written content = %q, want %q", string(data), srcContent)
		}
	})

	t.Run("WriteSession rejects wrong agent", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")

		session := &agent.AgentSession{
			SessionID:  "test",
			AgentName:  "other-agent", // Wrong agent
			SessionRef: "/tmp/test.jsonl",
			NativeData: []byte("data"),
		}

		err := ag.WriteSession(session)
		if err == nil {
			t.Error("WriteSession() should reject session from different agent")
		}
	})
}

// TestOpenCodeHelperMethods verifies OpenCode-specific helper methods.
func TestOpenCodeHelperMethods(t *testing.T) {
	t.Parallel()

	t.Run("FormatResumeCommand returns opencode --resume", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		cmd := ag.FormatResumeCommand("abc123")

		if cmd != "opencode --resume abc123" {
			t.Errorf("FormatResumeCommand() = %q, want %q", cmd, "opencode --resume abc123")
		}
	})

	t.Run("GetHookConfigPath returns .opencode/plugins/entire.ts", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		path := ag.GetHookConfigPath()

		if path != ".opencode/plugins/entire.ts" {
			t.Errorf("GetHookConfigPath() = %q, want %q", path, ".opencode/plugins/entire.ts")
		}
	})

	t.Run("Name returns opencode", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		if ag.Name() != "opencode" {
			t.Errorf("Name() = %q, want %q", ag.Name(), "opencode")
		}
	})

	t.Run("Type returns opencode type", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		if ag.Type() != agent.AgentTypeOpenCode {
			t.Errorf("Type() = %q, want %q", ag.Type(), agent.AgentTypeOpenCode)
		}
	})

	t.Run("SupportsHooks returns true", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		if !ag.SupportsHooks() {
			t.Error("SupportsHooks() = false, want true")
		}
	})

	t.Run("ProtectedDirs includes .opencode", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		dirs := ag.ProtectedDirs()

		found := false
		for _, d := range dirs {
			if d == ".opencode" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProtectedDirs() = %v, want to contain '.opencode'", dirs)
		}
	})

	t.Run("Description is non-empty", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		if ag.Description() == "" {
			t.Error("Description() should not be empty")
		}
	})

	t.Run("TranscriptAnalyzer - GetTranscriptPosition", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)

		ag, _ := agent.Get("opencode")
		analyzer, ok := ag.(agent.TranscriptAnalyzer)
		if !ok {
			t.Fatal("opencode agent does not implement TranscriptAnalyzer")
		}

		// Write a 3-line JSONL transcript
		path := filepath.Join(env.RepoDir, "pos-test.jsonl")
		lines := `{"info":{"id":"1","role":"user"},"parts":[]}
{"info":{"id":"2","role":"assistant"},"parts":[]}
{"info":{"id":"3","role":"user"},"parts":[]}
`
		if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
			t.Fatalf("failed to write test transcript: %v", err)
		}

		pos, err := analyzer.GetTranscriptPosition(path)
		if err != nil {
			t.Fatalf("GetTranscriptPosition() error = %v", err)
		}
		if pos != 3 {
			t.Errorf("GetTranscriptPosition() = %d, want 3", pos)
		}
	})

	t.Run("TranscriptAnalyzer - nonexistent file returns 0", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		analyzer := ag.(agent.TranscriptAnalyzer)

		pos, err := analyzer.GetTranscriptPosition("/nonexistent/path.jsonl")
		if err != nil {
			t.Fatalf("GetTranscriptPosition() error = %v (want nil for nonexistent)", err)
		}
		if pos != 0 {
			t.Errorf("GetTranscriptPosition(nonexistent) = %d, want 0", pos)
		}
	})

	t.Run("TranscriptChunker splits JSONL", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("opencode")
		chunker, ok := ag.(agent.TranscriptChunker)
		if !ok {
			t.Fatal("opencode agent does not implement TranscriptChunker")
		}

		// Create content that needs chunking
		line1 := `{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"hello"}]}`
		line2 := `{"info":{"id":"2","role":"assistant"},"parts":[{"type":"text","text":"world"}]}`
		content := []byte(line1 + "\n" + line2 + "\n")

		// Chunk with a size that forces a split (each line is ~78 bytes, use just over one line)
		chunks, err := chunker.ChunkTranscript(content, len(line1)+10)
		if err != nil {
			t.Fatalf("ChunkTranscript() error = %v", err)
		}
		if len(chunks) < 2 {
			t.Errorf("ChunkTranscript() produced %d chunks, want at least 2", len(chunks))
		}

		// Reassemble and verify
		reassembled, err := chunker.ReassembleTranscript(chunks)
		if err != nil {
			t.Fatalf("ReassembleTranscript() error = %v", err)
		}
		if string(reassembled) != string(content) {
			t.Errorf("reassembled content doesn't match original")
		}
	})

	t.Run("ExtractModifiedFiles from transcript", func(t *testing.T) {
		t.Parallel()

		transcript := []byte(`{"info":{"id":"1","sessionID":"s1","role":"user","time":{"created":1,"completed":2}},"parts":[{"type":"text","text":"create files"}]}
{"info":{"id":"2","sessionID":"s1","role":"assistant","time":{"created":3,"completed":4}},"parts":[{"type":"tool","tool":"file_write","filePath":"src/main.go","state":{"status":"completed","input":"test","output":"ok"}},{"type":"tool","tool":"file_write","filePath":"src/util.go","state":{"status":"completed","input":"test","output":"ok"}}]}
`)

		files := opencode.ExtractModifiedFiles(transcript)
		if len(files) != 2 {
			t.Errorf("ExtractModifiedFiles() = %v, want 2 files", files)
		}
	})
}

// TestOpenCodeHookParsing verifies hook input parsing via OpenCode agent interface.
func TestOpenCodeHookParsing(t *testing.T) {
	t.Parallel()

	ag, _ := agent.Get("opencode")

	tests := []struct {
		name     string
		hookType agent.HookType
		input    string
		wantID   string
		wantRef  string
	}{
		{
			name:     "SessionStart",
			hookType: agent.HookSessionStart,
			input:    `{"session_id":"sess-oc-1","session_ref":"","transcript_path":"","timestamp":"2025-01-01T00:00:00Z"}`,
			wantID:   "sess-oc-1",
			wantRef:  "",
		},
		{
			name:     "Stop with transcript_path",
			hookType: agent.HookStop,
			input:    `{"session_id":"sess-oc-2","session_ref":"/tmp/ref","transcript_path":"/tmp/transcript.jsonl","timestamp":"2025-01-01T00:00:00Z"}`,
			wantID:   "sess-oc-2",
			wantRef:  "/tmp/transcript.jsonl", // transcript_path overrides session_ref
		},
		{
			name:     "Stop with only session_ref",
			hookType: agent.HookStop,
			input:    `{"session_id":"sess-oc-3","session_ref":"/tmp/ref.jsonl","timestamp":"2025-01-01T00:00:00Z"}`,
			wantID:   "sess-oc-3",
			wantRef:  "/tmp/ref.jsonl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := newStringReader(tt.input)
			hookInput, err := ag.ParseHookInput(tt.hookType, reader)
			if err != nil {
				t.Fatalf("ParseHookInput() error = %v", err)
			}

			if hookInput.SessionID != tt.wantID {
				t.Errorf("SessionID = %q, want %q", hookInput.SessionID, tt.wantID)
			}
			if hookInput.SessionRef != tt.wantRef {
				t.Errorf("SessionRef = %q, want %q", hookInput.SessionRef, tt.wantRef)
			}
		})
	}
}

// TestOpenCodeTaskHookParsing verifies task-start and task-complete hook parsing.
func TestOpenCodeTaskHookParsing(t *testing.T) {
	t.Parallel()

	ag, _ := agent.Get("opencode")

	t.Run("TaskStart", func(t *testing.T) {
		t.Parallel()

		input := `{"session_id":"sess-1","transcript_path":"/tmp/t.jsonl","tool_use_id":"tool-abc","tool_input":{"subagent_type":"dev","description":"test task"},"timestamp":"2025-01-01T00:00:00Z"}`
		reader := newStringReader(input)

		hookInput, err := ag.ParseHookInput(agent.HookPreToolUse, reader)
		if err != nil {
			t.Fatalf("ParseHookInput(PreToolUse) error = %v", err)
		}

		if hookInput.ToolUseID != "tool-abc" {
			t.Errorf("ToolUseID = %q, want %q", hookInput.ToolUseID, "tool-abc")
		}
		if hookInput.SessionRef != "/tmp/t.jsonl" {
			t.Errorf("SessionRef = %q, want %q", hookInput.SessionRef, "/tmp/t.jsonl")
		}
	})

	t.Run("TaskComplete", func(t *testing.T) {
		t.Parallel()

		input := `{"session_id":"sess-1","transcript_path":"/tmp/t.jsonl","tool_use_id":"tool-xyz","tool_input":{},"tool_response":{},"timestamp":"2025-01-01T00:00:00Z"}`
		reader := newStringReader(input)

		hookInput, err := ag.ParseHookInput(agent.HookPostToolUse, reader)
		if err != nil {
			t.Fatalf("ParseHookInput(PostToolUse) error = %v", err)
		}

		if hookInput.ToolUseID != "tool-xyz" {
			t.Errorf("ToolUseID = %q, want %q", hookInput.ToolUseID, "tool-xyz")
		}
	})
}
