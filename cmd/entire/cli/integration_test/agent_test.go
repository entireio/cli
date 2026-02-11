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
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
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

	t.Run("TransformSessionID is identity function", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("claude-code")
		entireID := ag.TransformSessionID("abc123")

		// TransformSessionID is now an identity function
		if entireID != "abc123" {
			t.Errorf("TransformSessionID() = %q, want %q (identity function)", entireID, "abc123")
		}
	})

	t.Run("ExtractAgentSessionID handles legacy date prefix", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("claude-code")
		agentID := ag.ExtractAgentSessionID("2025-12-18-abc123")

		// Should still extract the agent ID from legacy format
		if agentID != "abc123" {
			t.Errorf("ExtractAgentSessionID() = %q, want %q", agentID, "abc123")
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

	t.Run("TransformSessionID is identity function", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("gemini")
		entireID := ag.TransformSessionID("abc123")

		// TransformSessionID is now an identity function
		if entireID != "abc123" {
			t.Errorf("TransformSessionID() = %q, want %q (identity function)", entireID, "abc123")
		}
	})

	t.Run("ExtractAgentSessionID handles legacy date prefix", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("gemini")
		agentID := ag.ExtractAgentSessionID("2025-12-18-abc123")

		// Should still extract the agent ID from legacy format
		if agentID != "abc123" {
			t.Errorf("ExtractAgentSessionID() = %q, want %q", agentID, "abc123")
		}
	})

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

func TestPiAgent_IsRegistered(t *testing.T) {
	t.Parallel()

	agents := agent.List()
	found := false
	for _, name := range agents {
		if name == agent.AgentNamePi {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("agent.List() = %v, want to contain %q", agents, agent.AgentNamePi)
	}
}

func TestPiAgent_DetectPresenceWhenPiDirExists(t *testing.T) {
	env := NewTestEnv(t)
	env.InitRepo()
	t.Chdir(env.RepoDir)

	if err := os.MkdirAll(filepath.Join(env.RepoDir, ".pi"), 0o755); err != nil {
		t.Fatalf("failed to create .pi directory: %v", err)
	}

	ag, err := agent.Get(agent.AgentNamePi)
	if err != nil {
		t.Fatalf("agent.Get(pi) error = %v", err)
	}

	present, err := ag.DetectPresence()
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if !present {
		t.Fatal("DetectPresence() = false, want true when .pi exists")
	}
}

func TestPiAgent_HookInstallUninstallLifecycle(t *testing.T) {
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")
	t.Chdir(env.RepoDir)

	ag, err := agent.Get(agent.AgentNamePi)
	if err != nil {
		t.Fatalf("agent.Get(pi) error = %v", err)
	}

	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		t.Fatal("pi agent does not implement HookSupport")
	}

	count, err := hookAgent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("InstallHooks() count = %d, want 1", count)
	}

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	if _, err := os.Stat(extPath); err != nil {
		t.Fatalf("expected extension scaffold at %s: %v", extPath, err)
	}

	if !hookAgent.AreHooksInstalled() {
		t.Fatal("AreHooksInstalled() = false, want true after install")
	}

	if err := hookAgent.UninstallHooks(); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	if _, err := os.Stat(extPath); !os.IsNotExist(err) {
		t.Fatalf("expected extension scaffold removed after uninstall")
	}
	if hookAgent.AreHooksInstalled() {
		t.Fatal("AreHooksInstalled() = true, want false after uninstall")
	}
}

func TestPiAgent_LocalDevHookScaffold(t *testing.T) {
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")
	t.Chdir(env.RepoDir)

	ag, err := agent.Get(agent.AgentNamePi)
	if err != nil {
		t.Fatalf("agent.Get(pi) error = %v", err)
	}

	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		t.Fatal("pi agent does not implement HookSupport")
	}

	if _, err := hookAgent.InstallHooks(true, true); err != nil {
		t.Fatalf("InstallHooks(localDev=true) error = %v", err)
	}

	extPath := filepath.Join(env.RepoDir, ".pi", "extensions", "entire", "index.ts")
	data, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatalf("failed to read extension scaffold: %v", err)
	}

	expectedMainPath := filepath.Join(env.RepoDir, "cmd", "entire", "main.go")
	content := string(data)
	if !strings.Contains(content, expectedMainPath) {
		t.Fatalf("local-dev scaffold missing absolute go main path %q", expectedMainPath)
	}
}

func TestPiAgent_HookInstallationVariants(t *testing.T) {
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")
	t.Chdir(env.RepoDir)

	ag, err := agent.Get(agent.AgentNamePi)
	if err != nil {
		t.Fatalf("agent.Get(pi) error = %v", err)
	}

	hookAgent := ag.(agent.HookSupport)

	count, err := hookAgent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("initial InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("initial InstallHooks() count = %d, want 1", count)
	}

	count, err = hookAgent.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("idempotent InstallHooks() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("idempotent InstallHooks() count = %d, want 0", count)
	}

	count, err = hookAgent.InstallHooks(false, true)
	if err != nil {
		t.Fatalf("force InstallHooks() error = %v", err)
	}
	if count != 0 && count != 1 {
		t.Fatalf("force InstallHooks() count = %d, want 0 or 1", count)
	}
	if !hookAgent.AreHooksInstalled() {
		t.Fatal("AreHooksInstalled() = false after force install")
	}
}

func TestPiAgent_SessionOperations(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()

	ag, err := agent.Get(agent.AgentNamePi)
	if err != nil {
		t.Fatalf("agent.Get(pi) error = %v", err)
	}

	srcPath := filepath.Join(env.RepoDir, "pi-session.jsonl")
	srcContent := `{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","arguments":{"path":"main.go"}}]}}
`
	if err := os.WriteFile(srcPath, []byte(srcContent), 0o644); err != nil {
		t.Fatalf("failed to write source transcript: %v", err)
	}

	session, err := ag.ReadSession(&agent.HookInput{SessionID: "pi-session", SessionRef: srcPath})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}
	if session.AgentName != agent.AgentNamePi {
		t.Fatalf("AgentName = %q, want %q", session.AgentName, agent.AgentNamePi)
	}
	if len(session.ModifiedFiles) != 1 || session.ModifiedFiles[0] != "main.go" {
		t.Fatalf("ModifiedFiles = %#v, want [main.go]", session.ModifiedFiles)
	}

	dstPath := filepath.Join(env.RepoDir, "pi-session-copy.jsonl")
	session.SessionRef = dstPath
	if err := ag.WriteSession(session); err != nil {
		t.Fatalf("WriteSession() error = %v", err)
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("failed to read copied transcript: %v", err)
	}
	if string(data) != srcContent {
		t.Fatalf("copied transcript mismatch")
	}

	err = ag.WriteSession(&agent.AgentSession{
		SessionID:  "wrong",
		AgentName:  agent.AgentNameGemini,
		SessionRef: filepath.Join(env.RepoDir, "wrong.jsonl"),
		NativeData: []byte("x"),
	})
	if err == nil {
		t.Fatal("WriteSession() expected error for wrong agent")
	}
}

func TestPiAgent_HelperMethods(t *testing.T) {
	t.Parallel()

	t.Run("identity transform and basic metadata", func(t *testing.T) {
		t.Parallel()

		ag, err := agent.Get(agent.AgentNamePi)
		if err != nil {
			t.Fatalf("agent.Get(pi) error = %v", err)
		}

		if got := ag.TransformSessionID("abc123"); got != "abc123" {
			t.Fatalf("TransformSessionID() = %q, want %q", got, "abc123")
		}
		if got := ag.ExtractAgentSessionID("abc123"); got != "abc123" {
			t.Fatalf("ExtractAgentSessionID() = %q, want %q", got, "abc123")
		}

		if got := ag.FormatResumeCommand("abc123"); !strings.Contains(got, "pi") {
			t.Fatalf("FormatResumeCommand() = %q, want to contain %q", got, "pi")
		}

		if got := ag.GetHookConfigPath(); got != "" {
			t.Fatalf("GetHookConfigPath() = %q, want empty", got)
		}
	})

	t.Run("GetLastUserPrompt extracts latest prompt", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		transcriptPath := filepath.Join(env.RepoDir, "pi-helper-last-user.jsonl")
		content := `{"type":"message","id":"1","message":{"role":"user","content":"first prompt"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
{"type":"message","id":"3","message":{"role":"user","content":[{"type":"text","text":"second"},{"type":"text","text":"prompt"}]}}
`
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get(agent.AgentNamePi)
		piAgent, ok := ag.(*pi.PiAgent)
		if !ok {
			t.Fatalf("agent type = %T, want *pi.PiAgent", ag)
		}

		session, err := ag.ReadSession(&agent.HookInput{SessionID: "s1", SessionRef: transcriptPath})
		if err != nil {
			t.Fatalf("ReadSession() error = %v", err)
		}

		prompt := piAgent.GetLastUserPrompt(session)
		if prompt != "second\n\nprompt" {
			t.Fatalf("GetLastUserPrompt() = %q, want %q", prompt, "second\n\nprompt")
		}
	})

	t.Run("TruncateAtUUID keeps root-to-target path on branched transcript", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		transcriptPath := filepath.Join(env.RepoDir, "pi-helper-truncate-tree.jsonl")
		content := `{"type":"session","version":3,"id":"sess","timestamp":"2026-01-01T00:00:00Z","cwd":"/repo"}
{"type":"message","id":"1","parentId":null,"message":{"role":"user","content":"root"}}
{"type":"message","id":"2","parentId":"1","message":{"role":"assistant","content":[{"type":"text","text":"branch"}]}}
{"type":"message","id":"3","parentId":"2","message":{"role":"user","content":"left"}}
{"type":"message","id":"4","parentId":"2","message":{"role":"user","content":"right"}}
`
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get(agent.AgentNamePi)
		piAgent, ok := ag.(*pi.PiAgent)
		if !ok {
			t.Fatalf("agent type = %T, want *pi.PiAgent", ag)
		}

		session, err := ag.ReadSession(&agent.HookInput{SessionID: "s1", SessionRef: transcriptPath})
		if err != nil {
			t.Fatalf("ReadSession() error = %v", err)
		}

		truncated, err := piAgent.TruncateAtUUID(session, "3")
		if err != nil {
			t.Fatalf("TruncateAtUUID() error = %v", err)
		}

		text := string(truncated.NativeData)
		if strings.Contains(text, `"id":"4"`) {
			t.Fatalf("expected sibling branch to be excluded, got: %s", text)
		}
		if !strings.Contains(text, `"id":"1"`) || !strings.Contains(text, `"id":"2"`) || !strings.Contains(text, `"id":"3"`) {
			t.Fatalf("expected root->target path to be preserved, got: %s", text)
		}
	})

	t.Run("FindCheckpointUUID finds tool result entry", func(t *testing.T) {
		t.Parallel()
		env := NewTestEnv(t)
		env.InitRepo()

		transcriptPath := filepath.Join(env.RepoDir, "pi-helper-checkpoint.jsonl")
		content := `{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-123","name":"write","arguments":{"path":"a.txt"}}]}}
{"type":"message","id":"2","message":{"role":"toolResult","toolName":"write","toolCallId":"tool-123","details":{"path":"a.txt"}}}
`
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		ag, _ := agent.Get(agent.AgentNamePi)
		piAgent, ok := ag.(*pi.PiAgent)
		if !ok {
			t.Fatalf("agent type = %T, want *pi.PiAgent", ag)
		}

		session, err := ag.ReadSession(&agent.HookInput{SessionID: "s1", SessionRef: transcriptPath})
		if err != nil {
			t.Fatalf("ReadSession() error = %v", err)
		}

		uuid, found := piAgent.FindCheckpointUUID(session, "tool-123")
		if !found {
			t.Fatal("FindCheckpointUUID() found = false, want true")
		}
		if uuid != "2" {
			t.Fatalf("FindCheckpointUUID() uuid = %q, want %q", uuid, "2")
		}
	})
}
