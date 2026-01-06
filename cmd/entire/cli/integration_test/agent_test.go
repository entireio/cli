//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/agent/claudecode"
)

// TestAgentDetection verifies agent detection and default behavior.
func TestAgentDetection(t *testing.T) {
	t.Parallel()

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
		t.Parallel()
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

		// Should install 6 hooks: SessionStart, Stop, UserPromptSubmit, PreToolUse[Task], PostToolUse[Task], PostToolUse[TodoWrite]
		if count != 6 {
			t.Errorf("InstallHooks() count = %d, want 6", count)
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

	t.Run("TransformSessionID adds date prefix", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("claude-code")
		entireID := ag.TransformSessionID("abc123")

		// Should have format YYYY-MM-DD-abc123
		if len(entireID) < 15 { // "2025-01-01-abc123" is 17 chars
			t.Errorf("TransformSessionID() = %q, too short", entireID)
		}
		if entireID[4] != '-' || entireID[7] != '-' || entireID[10] != '-' {
			t.Errorf("TransformSessionID() = %q, want date prefix format", entireID)
		}
		if entireID[11:] != "abc123" {
			t.Errorf("TransformSessionID() suffix = %q, want %q", entireID[11:], "abc123")
		}
	})

	t.Run("ExtractAgentSessionID removes date prefix", func(t *testing.T) {
		t.Parallel()

		ag, _ := agent.Get("claude-code")
		agentID := ag.ExtractAgentSessionID("2025-12-18-abc123")

		if agentID != "abc123" {
			t.Errorf("ExtractAgentSessionID() = %q, want %q", agentID, "abc123")
		}
	})
}
