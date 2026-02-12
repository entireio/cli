package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/spf13/cobra"
)

const testAgentName = "claude-code"

func TestNewAgentHookVerbCmd_LogsInvocation(t *testing.T) {
	// Setup: Create a temp directory with git repo structure
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo (required for paths.RepoRoot to work)
	gitInit := exec.CommandContext(context.Background(), "git", "init")
	if err := gitInit.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create .entire directory
	entireDir := filepath.Join(tmpDir, paths.EntireDir)
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create logs directory
	logsDir := filepath.Join(entireDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("failed to create logs directory: %v", err)
	}

	// Create session state file in .git/entire-sessions/
	sessionID := "test-claudecode-hook-session"
	writeTestSessionState(t, tmpDir, sessionID)

	// Enable debug logging
	t.Setenv(logging.LogLevelEnvVar, "DEBUG")

	// Initialize logging (normally done by PersistentPreRunE)
	cleanup := initHookLogging()
	defer cleanup()

	// Register a test handler
	testHandlerCalled := false
	RegisterHookHandler(agent.AgentName("test-agent"), "test-hook", func() error {
		testHandlerCalled = true
		return nil
	})

	// Create the command with logging
	cmd := newAgentHookVerbCmdWithLogging(agent.AgentName("test-agent"), "test-hook")

	// Execute the command
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("command execution failed: %v", err)
	}

	if !testHandlerCalled {
		t.Error("expected test handler to be called")
	}

	// Close logging to flush
	cleanup()

	// Verify log file was created and contains expected content
	logFile := filepath.Join(logsDir, "entire.log")
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	logContent := string(content)
	t.Logf("log content: %s", logContent)

	// Parse each log line as JSON
	lines := strings.Split(strings.TrimSpace(logContent), "\n")
	if len(lines) == 0 {
		t.Fatal("expected at least one log line")
	}

	// Check for hook invocation log
	foundInvocation := false
	foundCompletion := false
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("failed to parse log line as JSON: %v", err)
			continue
		}

		if entry["hook"] == "test-hook" {
			msg, msgOK := entry["msg"].(string)
			if !msgOK {
				continue
			}
			if strings.Contains(msg, "invoked") {
				foundInvocation = true
				// Verify component is set
				if entry["component"] != "hooks" {
					t.Errorf("expected component='hooks', got %v", entry["component"])
				}
				// Verify session_id is set
				if entry["session_id"] != sessionID {
					t.Errorf("expected session_id=%q, got %v", sessionID, entry["session_id"])
				}
			}
			if strings.Contains(msg, "completed") {
				foundCompletion = true
				// Verify duration_ms is present
				if _, ok := entry["duration_ms"]; !ok {
					t.Error("expected duration_ms in completion log")
				}
				// Verify success status
				if entry["success"] != true {
					t.Errorf("expected success=true, got %v", entry["success"])
				}
			}
		}
	}

	if !foundInvocation {
		t.Error("expected to find hook invocation log")
	}
	if !foundCompletion {
		t.Error("expected to find hook completion log")
	}
}

func TestNewAgentHookVerbCmd_LogsFailure(t *testing.T) {
	// Setup: Create a temp directory with git repo structure
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo (required for paths.RepoRoot to work)
	gitInit := exec.CommandContext(context.Background(), "git", "init")
	if err := gitInit.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create .entire directory and logs
	entireDir := filepath.Join(tmpDir, paths.EntireDir)
	logsDir := filepath.Join(entireDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("failed to create logs directory: %v", err)
	}

	// Create session state file in .git/entire-sessions/
	sessionID := "test-claudecode-failure-session"
	writeTestSessionState(t, tmpDir, sessionID)

	// Enable debug logging
	t.Setenv(logging.LogLevelEnvVar, "DEBUG")

	// Initialize logging (normally done by PersistentPreRunE)
	cleanup := initHookLogging()
	defer cleanup()

	// Register a handler that fails
	RegisterHookHandler(agent.AgentName("test-agent"), "failing-hook", func() error {
		return context.DeadlineExceeded // Use a real error
	})

	// Create the command with logging
	cmd := newAgentHookVerbCmdWithLogging(agent.AgentName("test-agent"), "failing-hook")
	cmd.SetOut(&bytes.Buffer{}) // Suppress output

	// Execute the command (expect error)
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("expected command to fail")
	}

	// Close logging to flush
	cleanup()

	// Verify log file contains failure status
	logFile := filepath.Join(logsDir, "entire.log")
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	logContent := string(content)
	lines := strings.Split(strings.TrimSpace(logContent), "\n")

	foundFailure := false
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		if entry["hook"] == "failing-hook" && entry["success"] == false {
			foundFailure = true
		}
	}

	if !foundFailure {
		t.Errorf("expected to find log entry with success=false, log content: %s", logContent)
	}
}

func TestClaudeCodeHooksCmd_HasLoggingHooks(t *testing.T) {
	// This test verifies that the claude-code hooks command has PersistentPreRunE
	// and PersistentPostRunE for logging initialization and cleanup

	// Get the actual hooks command which contains the claude-code subcommand
	hooksCmd := newHooksCmd()

	// Find the claude-code subcommand
	var claudeCodeCmd *cobra.Command
	for _, sub := range hooksCmd.Commands() {
		if sub.Use == testAgentName {
			claudeCodeCmd = sub
			break
		}
	}

	if claudeCodeCmd == nil {
		t.Fatal("expected to find claude-code subcommand under hooks")
	}

	// Verify PersistentPreRunE is set
	if claudeCodeCmd.PersistentPreRunE == nil {
		t.Error("expected PersistentPreRunE to be set for logging initialization")
	}

	// Verify PersistentPostRunE is set
	if claudeCodeCmd.PersistentPostRunE == nil {
		t.Error("expected PersistentPostRunE to be set for logging cleanup")
	}
}

func TestGeminiCLIHooksCmd_HasLoggingHooks(t *testing.T) {
	// This test verifies that the gemini hooks command has PersistentPreRunE
	// and PersistentPostRunE for logging initialization and cleanup

	// Get the actual hooks command which contains the gemini subcommand
	hooksCmd := newHooksCmd()

	// Find the gemini subcommand
	var geminiCmd *cobra.Command
	for _, sub := range hooksCmd.Commands() {
		if sub.Use == "gemini" {
			geminiCmd = sub
			break
		}
	}

	if geminiCmd == nil {
		t.Fatal("expected to find gemini subcommand under hooks")
	}

	// Verify PersistentPreRunE is set
	if geminiCmd.PersistentPreRunE == nil {
		t.Error("expected PersistentPreRunE to be set for logging initialization")
	}

	// Verify PersistentPostRunE is set
	if geminiCmd.PersistentPostRunE == nil {
		t.Error("expected PersistentPostRunE to be set for logging cleanup")
	}
}

func TestPiHooksCmd_HasLoggingHooks(t *testing.T) {
	// This test verifies that the pi hooks command has PersistentPreRunE
	// and PersistentPostRunE for logging initialization and cleanup

	hooksCmd := newHooksCmd()

	var piCmd *cobra.Command
	for _, sub := range hooksCmd.Commands() {
		if sub.Use == "pi" {
			piCmd = sub
			break
		}
	}

	if piCmd == nil {
		t.Fatal("expected to find pi subcommand under hooks")
	}

	if piCmd.PersistentPreRunE == nil {
		t.Error("expected PersistentPreRunE to be set for logging initialization")
	}

	if piCmd.PersistentPostRunE == nil {
		t.Error("expected PersistentPostRunE to be set for logging cleanup")
	}
}

func TestGetHookType_ToolHooks(t *testing.T) {
	tests := []struct {
		name string
		hook string
		want string
	}{
		{name: "gemini before-tool", hook: geminicli.HookNameBeforeTool, want: "tool"},
		{name: "gemini after-tool", hook: geminicli.HookNameAfterTool, want: "tool"},
		{name: "pi before-tool", hook: pi.HookNameBeforeTool, want: "tool"},
		{name: "pi after-tool", hook: pi.HookNameAfterTool, want: "tool"},
		{name: "claude pre-task", hook: "pre-task", want: "subagent"},
		{name: "agent hook", hook: "session-start", want: "agent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getHookType(tt.hook)
			if got != tt.want {
				t.Fatalf("getHookType(%q) = %q, want %q", tt.hook, got, tt.want)
			}
		})
	}
}

func TestHookCommand_SetsCurrentHookAgentName(t *testing.T) {
	// Verify that newAgentHookVerbCmdWithLogging sets currentHookAgentName
	// correctly for the handler, and clears it after

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo (required for paths.RepoRoot to work)
	gitInit := exec.CommandContext(context.Background(), "git", "init")
	if err := gitInit.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	tests := []struct {
		name      string
		agentName agent.AgentName
	}{
		{"claude-code hook sets claude-code", agent.AgentNameClaudeCode},
		{"gemini hook sets gemini", agent.AgentNameGemini},
		{"pi hook sets pi", agent.AgentNamePi},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var agentNameInsideHandler agent.AgentName

			hookName := "test-hook-" + string(tt.agentName)
			RegisterHookHandler(tt.agentName, hookName, func() error {
				agentNameInsideHandler = currentHookAgentName
				return nil
			})

			cmd := newAgentHookVerbCmdWithLogging(tt.agentName, hookName)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("command execution failed: %v", err)
			}

			// Inside handler, currentHookAgentName should match the agent
			if agentNameInsideHandler != tt.agentName {
				t.Errorf("inside handler: currentHookAgentName = %q, want %q", agentNameInsideHandler, tt.agentName)
			}

			// After handler completes, currentHookAgentName should be cleared
			if currentHookAgentName != "" {
				t.Errorf("after handler: currentHookAgentName = %q, want empty", currentHookAgentName)
			}
		})
	}
}

// writeTestSessionState creates a session state file in .git/entire-sessions/ for testing.
func writeTestSessionState(t *testing.T, repoDir, sessionID string) {
	t.Helper()
	stateDir := filepath.Join(repoDir, ".git", session.SessionStateDirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("failed to create session state directory: %v", err)
	}

	now := time.Now()
	state := session.State{
		SessionID:           sessionID,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("failed to marshal state: %v", err)
	}
	stateFile := filepath.Join(stateDir, sessionID+".json")
	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		t.Fatalf("failed to write session state file: %v", err)
	}
	t.Cleanup(func() { os.Remove(stateFile) })
}
