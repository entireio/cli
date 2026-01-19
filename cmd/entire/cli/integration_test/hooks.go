//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// HookRunner executes CLI hooks in the test environment.
type HookRunner struct {
	RepoDir          string
	ClaudeProjectDir string
	T                interface {
		Helper()
		Fatalf(format string, args ...interface{})
		Logf(format string, args ...interface{})
	}
}

// NewHookRunner creates a new hook runner for the given repo directory.
func NewHookRunner(repoDir, claudeProjectDir string, t interface {
	Helper()
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
}) *HookRunner {
	return &HookRunner{
		RepoDir:          repoDir,
		ClaudeProjectDir: claudeProjectDir,
		T:                t,
	}
}

// SimulateUserPromptSubmit simulates the UserPromptSubmit hook.
// This captures pre-prompt state (untracked files).
func (r *HookRunner) SimulateUserPromptSubmit(sessionID string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "", // Not used for user-prompt-submit
	}

	return r.runHookWithInput("user-prompt-submit", input)
}

// SimulateStop simulates the Stop hook with session transcript info.
func (r *HookRunner) SimulateStop(sessionID, transcriptPath string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	}

	return r.runHookWithInput("stop", input)
}

// PreTaskInput contains the input for PreToolUse[Task] hook.
type PreTaskInput struct {
	SessionID      string
	TranscriptPath string
	ToolUseID      string
	SubagentType   string // Optional: type of subagent (e.g., "dev", "reviewer")
	Description    string // Optional: task description
}

// SimulatePreTask simulates the PreToolUse[Task] hook.
func (r *HookRunner) SimulatePreTask(sessionID, transcriptPath, toolUseID string) error {
	r.T.Helper()

	return r.SimulatePreTaskWithInput(PreTaskInput{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		ToolUseID:      toolUseID,
	})
}

// SimulatePreTaskWithInput simulates the PreToolUse[Task] hook with full input.
func (r *HookRunner) SimulatePreTaskWithInput(input PreTaskInput) error {
	r.T.Helper()

	hookInput := map[string]interface{}{
		"session_id":      input.SessionID,
		"transcript_path": input.TranscriptPath,
		"tool_use_id":     input.ToolUseID,
		"tool_input": map[string]string{
			"subagent_type": input.SubagentType,
			"description":   input.Description,
		},
	}

	return r.runHookWithInput("pre-task", hookInput)
}

// PostTaskInput contains the input for PostToolUse[Task] hook.
type PostTaskInput struct {
	SessionID      string
	TranscriptPath string
	ToolUseID      string
	AgentID        string
}

// SimulatePostTask simulates the PostToolUse[Task] hook.
func (r *HookRunner) SimulatePostTask(input PostTaskInput) error {
	r.T.Helper()

	hookInput := map[string]interface{}{
		"session_id":      input.SessionID,
		"transcript_path": input.TranscriptPath,
		"tool_use_id":     input.ToolUseID,
		"tool_input":      map[string]string{},
		"tool_response": map[string]string{
			"agentId": input.AgentID,
		},
	}

	return r.runHookWithInput("post-task", hookInput)
}

func (r *HookRunner) runHookWithInput(flag string, input interface{}) error {
	r.T.Helper()

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("failed to marshal hook input: %w", err)
	}

	return r.runHookInRepoDir(flag, inputJSON)
}

func (r *HookRunner) runHookInRepoDir(hookName string, inputJSON []byte) error {
	// Run using the shared test binary
	// Command structure: entire hooks claude-code <hook-name>
	cmd := exec.Command(getTestBinary(), "hooks", "claude-code", hookName)
	cmd.Dir = r.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+r.ClaudeProjectDir,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hook %s failed: %w\nInput: %s\nOutput: %s",
			hookName, err, inputJSON, output)
	}

	r.T.Logf("Hook %s output: %s", hookName, output)
	return nil
}

// Session represents a simulated Claude Code session.
type Session struct {
	ID                string // Raw model session ID (e.g., "test-session-1")
	EntireID          string // Entire session ID with date prefix (e.g., "2025-12-02-test-session-1")
	TranscriptPath    string
	TranscriptBuilder *TranscriptBuilder
	env               *TestEnv
}

// FileChange represents a file modification in a session.
type FileChange struct {
	Path    string
	Content string
}

// NewSession creates a new simulated session.
func (env *TestEnv) NewSession() *Session {
	env.T.Helper()

	env.SessionCounter++
	sessionID := fmt.Sprintf("test-session-%d", env.SessionCounter)
	entireID := paths.EntireSessionID(sessionID)
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", sessionID+".jsonl")

	return &Session{
		ID:                sessionID,
		EntireID:          entireID,
		TranscriptPath:    transcriptPath,
		TranscriptBuilder: NewTranscriptBuilder(),
		env:               env,
	}
}

// CreateTranscript builds and writes a transcript for the session.
func (s *Session) CreateTranscript(prompt string, changes []FileChange) string {
	s.TranscriptBuilder.AddUserMessage(prompt)
	s.TranscriptBuilder.AddAssistantMessage("I'll help you with that.")

	for _, change := range changes {
		toolID := s.TranscriptBuilder.AddToolUse("mcp__acp__Write", change.Path, change.Content)
		s.TranscriptBuilder.AddToolResult(toolID)
	}

	s.TranscriptBuilder.AddAssistantMessage("Done!")

	if err := s.TranscriptBuilder.WriteToFile(s.TranscriptPath); err != nil {
		s.env.T.Fatalf("failed to write transcript: %v", err)
	}

	return s.TranscriptPath
}

// SimulateUserPromptSubmit is a convenience method on TestEnv.
func (env *TestEnv) SimulateUserPromptSubmit(sessionID string) error {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulateUserPromptSubmit(sessionID)
}

// SimulateStop is a convenience method on TestEnv.
func (env *TestEnv) SimulateStop(sessionID, transcriptPath string) error {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulateStop(sessionID, transcriptPath)
}

// SimulatePreTask is a convenience method on TestEnv.
func (env *TestEnv) SimulatePreTask(sessionID, transcriptPath, toolUseID string) error {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulatePreTask(sessionID, transcriptPath, toolUseID)
}

// SimulatePostTask is a convenience method on TestEnv.
func (env *TestEnv) SimulatePostTask(input PostTaskInput) error {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulatePostTask(input)
}

// Todo represents a single todo item in the PostTodoInput.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
}

// PostTodoInput contains the input for PostToolUse[TodoWrite] hook.
type PostTodoInput struct {
	SessionID      string
	TranscriptPath string
	ToolUseID      string // The TodoWrite tool use ID
	Todos          []Todo // The todo list
}

// SimulatePostTodo simulates the PostToolUse[TodoWrite] hook.
func (r *HookRunner) SimulatePostTodo(input PostTodoInput) error {
	r.T.Helper()

	hookInput := map[string]interface{}{
		"session_id":      input.SessionID,
		"transcript_path": input.TranscriptPath,
		"tool_name":       "TodoWrite",
		"tool_use_id":     input.ToolUseID,
		"tool_input": map[string]interface{}{
			"todos": input.Todos,
		},
		"tool_response": map[string]interface{}{},
	}

	return r.runHookWithInput("post-todo", hookInput)
}

// SimulatePostTodo is a convenience method on TestEnv.
func (env *TestEnv) SimulatePostTodo(input PostTodoInput) error {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulatePostTodo(input)
}

// ClearSessionState removes the session state file for the given session ID.
// This simulates what happens when a user commits their changes (session is "completed").
// Used in tests to allow sequential sessions to run without triggering concurrent session warnings.
func (env *TestEnv) ClearSessionState(sessionID string) error {
	env.T.Helper()

	// Session state is stored in .git/entire-sessions/<session-id>.json
	// Use paths.EntireSessionID to get the full session ID with date prefix
	entireSessionID := paths.EntireSessionID(sessionID)
	stateFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", entireSessionID+".json")

	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to clear session state: %w", err)
	}
	return nil
}

// CountSessionStateFiles returns the number of session state files in .git/entire-sessions/.
// Only counts .json files, excluding README.md and other non-session files.
func (env *TestEnv) CountSessionStateFiles() (int, error) {
	env.T.Helper()

	sessionStateDir := filepath.Join(env.RepoDir, ".git", "entire-sessions")
	entries, err := os.ReadDir(sessionStateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count, nil
}

// HookOutput contains the result of running a hook.
type HookOutput struct {
	Stdout []byte
	Stderr []byte
	Err    error
}

// runHookWithOutput runs a hook and returns both stdout and stderr separately.
func (r *HookRunner) runHookWithOutput(hookName string, inputJSON []byte) HookOutput {
	cmd := exec.Command(getTestBinary(), "hooks", "claude-code", hookName)
	cmd.Dir = r.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+r.ClaudeProjectDir,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return HookOutput{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
		Err:    err,
	}
}

// SimulateUserPromptSubmitWithOutput simulates the UserPromptSubmit hook and returns the output.
func (r *HookRunner) SimulateUserPromptSubmitWithOutput(sessionID string) HookOutput {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return HookOutput{Err: fmt.Errorf("failed to marshal hook input: %w", err)}
	}

	return r.runHookWithOutput("user-prompt-submit", inputJSON)
}

// SimulateUserPromptSubmitWithOutput is a convenience method on TestEnv.
func (env *TestEnv) SimulateUserPromptSubmitWithOutput(sessionID string) HookOutput {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulateUserPromptSubmitWithOutput(sessionID)
}

// GetSessionState reads and returns the session state for the given session ID.
func (env *TestEnv) GetSessionState(sessionID string) (*strategy.SessionState, error) {
	env.T.Helper()

	entireSessionID := paths.EntireSessionID(sessionID)
	stateFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", entireSessionID+".json")

	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read session state: %w", err)
	}

	var state strategy.SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse session state: %w", err)
	}
	return &state, nil
}
