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
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"

	"github.com/spf13/cobra"
)

const testAgentName = "claude-code"

func TestNewAgentHookVerbCmd_LogsInvocation(t *testing.T) {
	// Setup: Create a temp directory with git repo structure
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo (required for paths.WorktreeRoot to work)
	gitInit := exec.CommandContext(context.Background(), "git", "init")
	if err := gitInit.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit so repository is not empty
	gitConfig := exec.CommandContext(context.Background(), "git", "config", "user.email", "test@test.com")
	if err := gitConfig.Run(); err != nil {
		t.Fatalf("failed to configure git user.email: %v", err)
	}
	gitConfigName := exec.CommandContext(context.Background(), "git", "config", "user.name", "Test User")
	if err := gitConfigName.Run(); err != nil {
		t.Fatalf("failed to configure git user.name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to create README: %v", err)
	}
	gitAdd := exec.CommandContext(context.Background(), "git", "add", "README.md")
	if err := gitAdd.Run(); err != nil {
		t.Fatalf("failed to git add: %v", err)
	}
	gitCommit := exec.CommandContext(context.Background(), "git", "commit", "-m", "Initial commit")
	gitCommit.Env = testutil.GitIsolatedEnv()
	if err := gitCommit.Run(); err != nil {
		t.Fatalf("failed to git commit: %v", err)
	}

	// Create .entire directory
	entireDir := filepath.Join(tmpDir, paths.EntireDir)
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create settings.json to indicate Entire is set up in this repo
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled":true,"strategy":"manual-commit"}`), 0o644); err != nil {
		t.Fatalf("failed to create settings file: %v", err)
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
	cleanup := initHookLogging(context.Background())
	defer cleanup()

	// Create a transcript file for the hook input
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"content":"test"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("failed to create transcript file: %v", err)
	}

	// Create stdin with session-start hook input
	hookInput := map[string]interface{}{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	}
	inputJSON, _ := json.Marshal(hookInput) //nolint:errcheck,errchkjson // Test code; JSON marshal of simple map never fails

	// Create the command with logging - use session-start hook which is a pass-through
	cmd := newAgentHookVerbCmdWithLogging(agent.AgentNameClaudeCode, claudecode.HookNameSessionStart)

	// Set stdin
	cmd.SetIn(bytes.NewReader(inputJSON))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	// Execute the command
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("command execution failed: %v", err)
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

	// Check for hook invocation log and perf span log
	foundInvocation := false
	foundPerfSpan := false
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("failed to parse log line as JSON: %v", err)
			continue
		}

		msg, msgOK := entry["msg"].(string)

		// Hook invocation log: msg="hook invoked", hook="session-start"
		if msgOK && entry["hook"] == claudecode.HookNameSessionStart && strings.Contains(msg, "invoked") {
			foundInvocation = true
			// Verify component is set
			if entry["component"] != "hooks" {
				t.Errorf("expected component='hooks', got %v", entry["component"])
			}
		}

		// Perf span log: msg="perf", op="session-start", duration_ms present
		if msgOK && msg == "perf" && entry["op"] == claudecode.HookNameSessionStart {
			foundPerfSpan = true
			if _, ok := entry["duration_ms"]; !ok {
				t.Error("expected duration_ms in perf span log")
			}
		}
	}

	if !foundInvocation {
		t.Error("expected to find hook invocation log")
	}
	if !foundPerfSpan {
		t.Error("expected to find perf span log")
	}
}

func TestExecuteAgentHookSessionStartSkipsCaptureWhenPolicyUnsupported(t *testing.T) {
	setupStopTestRepo(t)
	repoRoot := mustGetwd(t)
	enableEntire(t, repoRoot)

	repo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	writeUnsupportedCheckpointPolicyForCLITest(t, repo)

	sessionID := "policy-session-start"
	payload, err := json.Marshal(map[string]string{
		"session_id":      sessionID,
		"transcript_path": filepath.Join(repoRoot, "transcript.jsonl"),
	})
	require.NoError(t, err)

	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(payload))
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	require.NoError(t, executeAgentHook(cmd, agent.AgentNameClaudeCode, claudecode.HookNameSessionStart, false))

	hintPath := filepath.Join(repoRoot, ".git", session.SessionStateDirName, sessionID+".agent")
	_, statErr := os.Stat(hintPath)
	require.True(t, os.IsNotExist(statErr), "session-start must not claim the session when checkpoint policy is unsupported")
}

func TestExecuteAgentHookSessionStartSkipsCaptureWhenPolicyUnreadable(t *testing.T) {
	setupStopTestRepo(t)
	repoRoot := mustGetwd(t)
	enableEntire(t, repoRoot)

	repo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	writeMalformedCheckpointPolicyForCLITest(t, repo)

	sessionID := "policy-unreadable-session-start"
	payload, err := json.Marshal(map[string]string{
		"session_id":      sessionID,
		"transcript_path": filepath.Join(repoRoot, "transcript.jsonl"),
	})
	require.NoError(t, err)

	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(payload))
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	require.NoError(t, executeAgentHook(cmd, agent.AgentNameClaudeCode, claudecode.HookNameSessionStart, false))

	hintPath := filepath.Join(repoRoot, ".git", session.SessionStateDirName, sessionID+".agent")
	_, statErr := os.Stat(hintPath)
	require.True(t, os.IsNotExist(statErr), "session-start must not claim the session when checkpoint policy is unreadable")
}

func TestAgentHookPolicyFailsWhenRepoCannotOpen(t *testing.T) {
	_, err := agentHookPolicy(context.Background(), filepath.Join(t.TempDir(), "missing"))

	require.ErrorIs(t, err, errUnreadableCheckpointPolicy)
	require.Contains(t, err.Error(), "failed to open repository")
}

func TestShouldSkipAgentHookForPolicy(t *testing.T) {
	t.Parallel()

	require.False(t, shouldSkipAgentHookForPolicy(checkpointpolicy.DefaultPolicy()))
	require.True(t, shouldSkipAgentHookForPolicy(checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "branch-v1",
	}))
}

func TestHookWritesCheckpointData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                         string
		eventType                    agent.EventType
		claudePostTodoCheckpointHook bool
		want                         bool
	}{
		{name: "session start warns only", eventType: agent.SessionStart},
		{name: "turn start initializes session", eventType: agent.TurnStart},
		{name: "turn end writes session checkpoint", eventType: agent.TurnEnd, want: true},
		{name: "compaction updates state only", eventType: agent.Compaction},
		{name: "session end updates state", eventType: agent.SessionEnd},
		{name: "subagent start captures pre-task state", eventType: agent.SubagentStart},
		{name: "subagent end writes task checkpoint", eventType: agent.SubagentEnd, want: true},
		{name: "model update stores hint", eventType: agent.ModelUpdate},
		{name: "tool use records files touched", eventType: agent.ToolUse},
		{name: "claude post todo writes incremental checkpoint", claudePostTodoCheckpointHook: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, hookWritesCheckpointData(tt.eventType, tt.claudePostTodoCheckpointHook))
		})
	}
}

func TestAgentWriteHookLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                         string
		eventType                    agent.EventType
		claudePostTodoCheckpointHook bool
		want                         string
	}{
		{name: "post todo takes priority", claudePostTodoCheckpointHook: true, want: "post-todo"},
		{name: "subagent end", eventType: agent.SubagentEnd, want: "subagent-end"},
		{name: "turn end is the default", eventType: agent.TurnEnd, want: "turn-end"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, agentWriteHookLabel(tt.eventType, tt.claudePostTodoCheckpointHook))
		})
	}
}

func TestExecuteAgentHookTurnStartDispatchesWhenPolicyUnsupported(t *testing.T) {
	setupStopTestRepo(t)
	repoRoot := mustGetwd(t)
	enableEntire(t, repoRoot)

	repo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	writeUnsupportedCheckpointPolicyForCLITest(t, repo)

	transcriptPath := filepath.Join(repoRoot, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600))
	payload, err := json.Marshal(map[string]string{
		"session_id":      "policy-turn-start",
		"transcript_path": transcriptPath,
		"prompt":          "hello",
	})
	require.NoError(t, err)

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(payload))
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	err = executeAgentHook(cmd, agent.AgentNameClaudeCode, claudecode.HookNameUserPromptSubmit, false)
	require.NoError(t, err)
	require.NotContains(t, stderr.String(), "Checkpoint capture is disabled for this repository.")

	state, err := strategy.LoadSessionState(context.Background(), "policy-turn-start")
	require.NoError(t, err)
	require.NotNil(t, state, "TurnStart must dispatch so InitializeSession can create session state")
}

func TestExecuteAgentHookTurnStartDispatchesWhenPolicyUnreadable(t *testing.T) {
	setupStopTestRepo(t)
	repoRoot := mustGetwd(t)
	enableEntire(t, repoRoot)

	repo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	writeMalformedCheckpointPolicyForCLITest(t, repo)

	transcriptPath := filepath.Join(repoRoot, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600))
	payload, err := json.Marshal(map[string]string{
		"session_id":      "policy-unreadable-turn-start",
		"transcript_path": transcriptPath,
		"prompt":          "hello",
	})
	require.NoError(t, err)

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(payload))
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	err = executeAgentHook(cmd, agent.AgentNameClaudeCode, claudecode.HookNameUserPromptSubmit, false)
	require.NoError(t, err)
	require.NotContains(t, stderr.String(), "Checkpoint capture is disabled for this repository.")

	state, err := strategy.LoadSessionState(context.Background(), "policy-unreadable-turn-start")
	require.NoError(t, err)
	require.NotNil(t, state, "TurnStart must dispatch so InitializeSession can create session state")
}

func TestExecuteAgentHookPostTodoFailsWhenPolicyUnsupported(t *testing.T) {
	setupStopTestRepo(t)
	repoRoot := mustGetwd(t)
	enableEntire(t, repoRoot)

	repo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	writeUnsupportedCheckpointPolicyForCLITest(t, repo)

	payload, err := json.Marshal(map[string]any{
		"session_id":      "policy-post-todo",
		"transcript_path": filepath.Join(repoRoot, "transcript.jsonl"),
		"tool_name":       "TodoWrite",
		"tool_use_id":     "tool-1",
		"tool_input":      map[string]any{"todos": []any{}},
		"tool_response":   map[string]any{},
	})
	require.NoError(t, err)

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(payload))
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	err = executeAgentHook(cmd, agent.AgentNameClaudeCode, claudecode.HookNamePostTodo, false)
	require.Error(t, err)
	require.Contains(t, stderr.String(), "Checkpoint capture is disabled for this repository.")
	require.Contains(t, stderr.String(), "No Entire checkpoints will be created until the CLI is upgraded.")
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

	require.NotNil(t, claudeCodeCmd, "expected to find claude-code subcommand under hooks")

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

	require.NotNil(t, geminiCmd, "expected to find gemini subcommand under hooks")

	// Verify PersistentPreRunE is set
	if geminiCmd.PersistentPreRunE == nil {
		t.Error("expected PersistentPreRunE to be set for logging initialization")
	}

	// Verify PersistentPostRunE is set
	if geminiCmd.PersistentPostRunE == nil {
		t.Error("expected PersistentPostRunE to be set for logging cleanup")
	}
}

func TestHookCommand_SetsCurrentHookAgentName(t *testing.T) {
	// Verify that newAgentHookVerbCmdWithLogging sets currentHookAgentName
	// correctly for the handler, and clears it after

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo (required for paths.WorktreeRoot to work)
	gitInit := exec.CommandContext(context.Background(), "git", "init")
	if err := gitInit.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit so repository is not empty
	gitConfig := exec.CommandContext(context.Background(), "git", "config", "user.email", "test@test.com")
	if err := gitConfig.Run(); err != nil {
		t.Fatalf("failed to configure git user.email: %v", err)
	}
	gitConfigName := exec.CommandContext(context.Background(), "git", "config", "user.name", "Test User")
	if err := gitConfigName.Run(); err != nil {
		t.Fatalf("failed to configure git user.name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to create README: %v", err)
	}
	gitAdd := exec.CommandContext(context.Background(), "git", "add", "README.md")
	if err := gitAdd.Run(); err != nil {
		t.Fatalf("failed to git add: %v", err)
	}
	gitCommit := exec.CommandContext(context.Background(), "git", "commit", "-m", "Initial commit")
	gitCommit.Env = testutil.GitIsolatedEnv()
	if err := gitCommit.Run(); err != nil {
		t.Fatalf("failed to git commit: %v", err)
	}

	// Create .entire directory to enable Entire
	entireDir := filepath.Join(tmpDir, paths.EntireDir)
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create session state file
	sessionID := "test-agent-name-session"
	writeTestSessionState(t, tmpDir, sessionID)

	// Create transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"content":"test"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("failed to create transcript file: %v", err)
	}

	// Create stdin input
	hookInput := map[string]interface{}{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	}
	inputJSON, _ := json.Marshal(hookInput) //nolint:errcheck,errchkjson // Test code; JSON marshal of simple map never fails

	// Test with Claude Code using session-start hook (pass-through but sets agent name)
	cmd := newAgentHookVerbCmdWithLogging(agent.AgentNameClaudeCode, claudecode.HookNameSessionStart)
	cmd.SetIn(bytes.NewReader(inputJSON))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("command execution failed: %v", err)
	}

	// After handler completes, currentHookAgentName should be cleared
	if currentHookAgentName != "" {
		t.Errorf("after handler: currentHookAgentName = %q, want empty", currentHookAgentName)
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
