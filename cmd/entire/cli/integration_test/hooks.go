//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/sessionid"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
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

// HookResponse represents the JSON response from Claude Code hooks.
type HookResponse struct {
	Continue   bool   `json:"continue"`
	StopReason string `json:"stopReason,omitempty"`
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

// SimulateUserPromptSubmitWithResponse simulates the UserPromptSubmit hook
// and returns the parsed hook response (for testing blocking behavior).
func (r *HookRunner) SimulateUserPromptSubmitWithResponse(sessionID string) (*HookResponse, error) {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "", // Not used for user-prompt-submit
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal hook input: %w", err)
	}

	output := r.runHookWithOutput("user-prompt-submit", inputJSON)

	// If hook failed with an error, return the error
	if output.Err != nil {
		return nil, fmt.Errorf("hook failed: %w\nStderr: %s\nStdout: %s",
			output.Err, output.Stderr, output.Stdout)
	}

	// Parse JSON response from stdout
	var resp HookResponse
	if len(output.Stdout) > 0 {
		if err := json.Unmarshal(output.Stdout, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse hook response: %w\nStdout: %s",
				err, output.Stdout)
		}
	}

	return &resp, nil
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
	entireID := sessionid.EntireSessionID(sessionID)
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

// SimulateUserPromptSubmitWithResponse is a convenience method on TestEnv.
func (env *TestEnv) SimulateUserPromptSubmitWithResponse(sessionID string) (*HookResponse, error) {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulateUserPromptSubmitWithResponse(sessionID)
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
	// Use sessionid.EntireSessionID to get the full session ID with date prefix
	entireSessionID := sessionid.EntireSessionID(sessionID)
	stateFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", entireSessionID+".json")

	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to clear session state: %w", err)
	}
	return nil
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

// SimulateSessionStartWithOutput simulates the SessionStart hook and returns the output.
func (r *HookRunner) SimulateSessionStartWithOutput(sessionID string) HookOutput {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return HookOutput{Err: fmt.Errorf("failed to marshal hook input: %w", err)}
	}

	return r.runHookWithOutput("session-start", inputJSON)
}

// SimulateSessionStartWithOutput is a convenience method on TestEnv.
func (env *TestEnv) SimulateSessionStartWithOutput(sessionID string) HookOutput {
	env.T.Helper()
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T)
	return runner.SimulateSessionStartWithOutput(sessionID)
}

// GetSessionState reads and returns the session state for the given session ID.
func (env *TestEnv) GetSessionState(sessionID string) (*strategy.SessionState, error) {
	env.T.Helper()

	entireSessionID := sessionid.EntireSessionID(sessionID)
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

// GeminiHookRunner executes Gemini CLI hooks in the test environment.
type GeminiHookRunner struct {
	RepoDir          string
	GeminiProjectDir string
	T                interface {
		Helper()
		Fatalf(format string, args ...interface{})
		Logf(format string, args ...interface{})
	}
}

// NewGeminiHookRunner creates a new Gemini hook runner for the given repo directory.
func NewGeminiHookRunner(repoDir, geminiProjectDir string, t interface {
	Helper()
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
}) *GeminiHookRunner {
	return &GeminiHookRunner{
		RepoDir:          repoDir,
		GeminiProjectDir: geminiProjectDir,
		T:                t,
	}
}

// runGeminiHookWithInput runs a Gemini hook with the given input.
func (r *GeminiHookRunner) runGeminiHookWithInput(hookName string, input interface{}) error {
	r.T.Helper()

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("failed to marshal hook input: %w", err)
	}

	return r.runGeminiHookInRepoDir(hookName, inputJSON)
}

func (r *GeminiHookRunner) runGeminiHookInRepoDir(hookName string, inputJSON []byte) error {
	// Run using the shared test binary
	// Command structure: entire hooks gemini <hook-name>
	cmd := exec.Command(getTestBinary(), "hooks", "gemini", hookName)
	cmd.Dir = r.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+r.GeminiProjectDir,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hook %s failed: %w\nInput: %s\nOutput: %s",
			hookName, err, inputJSON, output)
	}

	r.T.Logf("Gemini hook %s output: %s", hookName, output)
	return nil
}

// runGeminiHookWithOutput runs a Gemini hook and returns both stdout and stderr separately.
func (r *GeminiHookRunner) runGeminiHookWithOutput(hookName string, inputJSON []byte) HookOutput {
	cmd := exec.Command(getTestBinary(), "hooks", "gemini", hookName)
	cmd.Dir = r.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_GEMINI_PROJECT_DIR="+r.GeminiProjectDir,
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

// SimulateGeminiBeforeAgent simulates the BeforeAgent hook for Gemini CLI.
// This is equivalent to Claude Code's UserPromptSubmit.
func (r *GeminiHookRunner) SimulateGeminiBeforeAgent(sessionID string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
		"cwd":             r.RepoDir,
		"hook_event_name": "BeforeAgent",
		"timestamp":       "2025-01-01T00:00:00Z",
		"prompt":          "test prompt",
	}

	return r.runGeminiHookWithInput("before-agent", input)
}

// SimulateGeminiBeforeAgentWithOutput simulates the BeforeAgent hook and returns the output.
func (r *GeminiHookRunner) SimulateGeminiBeforeAgentWithOutput(sessionID string) HookOutput {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
		"cwd":             r.RepoDir,
		"hook_event_name": "BeforeAgent",
		"timestamp":       "2025-01-01T00:00:00Z",
		"prompt":          "test prompt",
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return HookOutput{Err: fmt.Errorf("failed to marshal hook input: %w", err)}
	}

	return r.runGeminiHookWithOutput("before-agent", inputJSON)
}

// SimulateGeminiAfterAgent simulates the AfterAgent hook for Gemini CLI.
// This is the primary checkpoint creation hook, equivalent to Claude Code's Stop hook.
func (r *GeminiHookRunner) SimulateGeminiAfterAgent(sessionID, transcriptPath string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"cwd":             r.RepoDir,
		"hook_event_name": "AfterAgent",
		"timestamp":       "2025-01-01T00:00:00Z",
	}

	return r.runGeminiHookWithInput("after-agent", input)
}

// SimulateGeminiSessionEnd simulates the SessionEnd hook for Gemini CLI.
// This is a cleanup/fallback hook that fires on explicit exit.
func (r *GeminiHookRunner) SimulateGeminiSessionEnd(sessionID, transcriptPath string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"cwd":             r.RepoDir,
		"hook_event_name": "SessionEnd",
		"timestamp":       "2025-01-01T00:00:00Z",
		"reason":          "exit",
	}

	return r.runGeminiHookWithInput("session-end", input)
}

// GeminiSession represents a simulated Gemini CLI session.
type GeminiSession struct {
	ID             string // Raw model session ID (e.g., "gemini-session-1")
	EntireID       string // Entire session ID with date prefix (e.g., "2025-12-02-gemini-session-1")
	TranscriptPath string
	env            *TestEnv
}

// NewGeminiSession creates a new simulated Gemini session.
func (env *TestEnv) NewGeminiSession() *GeminiSession {
	env.T.Helper()

	env.SessionCounter++
	sessionID := fmt.Sprintf("gemini-session-%d", env.SessionCounter)
	entireID := sessionid.EntireSessionID(sessionID)
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", sessionID+".json")

	return &GeminiSession{
		ID:             sessionID,
		EntireID:       entireID,
		TranscriptPath: transcriptPath,
		env:            env,
	}
}

// CreateGeminiTranscript creates a Gemini JSON transcript file for the session.
func (s *GeminiSession) CreateGeminiTranscript(prompt string, changes []FileChange) string {
	// Build Gemini-format transcript (JSON, not JSONL)
	messages := []map[string]interface{}{
		{
			"type":    "user",
			"content": prompt,
		},
		{
			"type":    "assistant",
			"content": "I'll help you with that.",
		},
	}

	for _, change := range changes {
		messages = append(messages, map[string]interface{}{
			"type": "tool_use",
			"name": "write_file",
			"input": map[string]string{
				"path":    change.Path,
				"content": change.Content,
			},
		})
		messages = append(messages, map[string]interface{}{
			"type":   "tool_result",
			"output": "File written successfully",
		})
	}

	messages = append(messages, map[string]interface{}{
		"type":    "assistant",
		"content": "Done!",
	})

	transcript := map[string]interface{}{
		"sessionId": s.ID,
		"messages":  messages,
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(s.TranscriptPath), 0o755); err != nil {
		s.env.T.Fatalf("failed to create transcript dir: %v", err)
	}

	// Write transcript
	data, err := json.MarshalIndent(transcript, "", "  ")
	if err != nil {
		s.env.T.Fatalf("failed to marshal transcript: %v", err)
	}
	if err := os.WriteFile(s.TranscriptPath, data, 0o644); err != nil {
		s.env.T.Fatalf("failed to write transcript: %v", err)
	}

	return s.TranscriptPath
}

// SimulateGeminiBeforeAgent is a convenience method on TestEnv.
func (env *TestEnv) SimulateGeminiBeforeAgent(sessionID string) error {
	env.T.Helper()
	runner := NewGeminiHookRunner(env.RepoDir, env.GeminiProjectDir, env.T)
	return runner.SimulateGeminiBeforeAgent(sessionID)
}

// SimulateGeminiBeforeAgentWithOutput is a convenience method on TestEnv.
func (env *TestEnv) SimulateGeminiBeforeAgentWithOutput(sessionID string) HookOutput {
	env.T.Helper()
	runner := NewGeminiHookRunner(env.RepoDir, env.GeminiProjectDir, env.T)
	return runner.SimulateGeminiBeforeAgentWithOutput(sessionID)
}

// SimulateGeminiAfterAgent is a convenience method on TestEnv.
func (env *TestEnv) SimulateGeminiAfterAgent(sessionID, transcriptPath string) error {
	env.T.Helper()
	runner := NewGeminiHookRunner(env.RepoDir, env.GeminiProjectDir, env.T)
	return runner.SimulateGeminiAfterAgent(sessionID, transcriptPath)
}

// SimulateGeminiSessionEnd is a convenience method on TestEnv.
func (env *TestEnv) SimulateGeminiSessionEnd(sessionID, transcriptPath string) error {
	env.T.Helper()
	runner := NewGeminiHookRunner(env.RepoDir, env.GeminiProjectDir, env.T)
	return runner.SimulateGeminiSessionEnd(sessionID, transcriptPath)
}

// PiHookRunner executes Pi hooks in the test environment.
type PiHookRunner struct {
	RepoDir string
	T       interface {
		Helper()
		Fatalf(format string, args ...interface{})
		Logf(format string, args ...interface{})
	}
}

// NewPiHookRunner creates a new Pi hook runner for the given repo directory.
func NewPiHookRunner(repoDir string, t interface {
	Helper()
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
}) *PiHookRunner {
	return &PiHookRunner{RepoDir: repoDir, T: t}
}

func (r *PiHookRunner) runPiHookWithInput(hookName string, input interface{}) error {
	r.T.Helper()

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("failed to marshal hook input: %w", err)
	}

	return r.runPiHookInRepoDir(hookName, inputJSON)
}

func (r *PiHookRunner) runPiHookInRepoDir(hookName string, inputJSON []byte) error {
	cmd := exec.Command(getTestBinary(), "hooks", "pi", hookName)
	cmd.Dir = r.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hook %s failed: %w\nInput: %s\nOutput: %s",
			hookName, err, inputJSON, output)
	}

	r.T.Logf("Pi hook %s output: %s", hookName, output)
	return nil
}

// runPiHookWithOutput runs a Pi hook and returns both stdout and stderr separately.
func (r *PiHookRunner) runPiHookWithOutput(hookName string, inputJSON []byte) HookOutput {
	cmd := exec.Command(getTestBinary(), "hooks", "pi", hookName)
	cmd.Dir = r.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)

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

// SimulatePiSessionStart simulates the session-start hook for Pi.
func (r *PiHookRunner) SimulatePiSessionStart(sessionID string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
	}

	return r.runPiHookWithInput("session-start", input)
}

// SimulatePiSessionStartWithOutput simulates session-start and returns output.
func (r *PiHookRunner) SimulatePiSessionStartWithOutput(sessionID string) HookOutput {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return HookOutput{Err: fmt.Errorf("failed to marshal hook input: %w", err)}
	}

	return r.runPiHookWithOutput("session-start", inputJSON)
}

// SimulatePiUserPromptSubmit simulates the user-prompt-submit hook for Pi.
func (r *PiHookRunner) SimulatePiUserPromptSubmit(sessionID string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": "",
		"prompt":          "test prompt",
	}

	return r.runPiHookWithInput("user-prompt-submit", input)
}

// SimulatePiBeforeTool simulates the before-tool hook for Pi.
func (r *PiHookRunner) SimulatePiBeforeTool(sessionID, transcriptPath, toolName string, toolInput any) error {
	r.T.Helper()

	input := map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"tool_name":       toolName,
		"tool_input":      toolInput,
	}

	return r.runPiHookWithInput("before-tool", input)
}

// SimulatePiAfterTool simulates the after-tool hook for Pi.
func (r *PiHookRunner) SimulatePiAfterTool(sessionID, transcriptPath, toolName string, toolInput any, toolResponse any) error {
	r.T.Helper()

	input := map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"tool_response":   toolResponse,
	}

	return r.runPiHookWithInput("after-tool", input)
}

// SimulatePiStop simulates the stop hook for Pi.
func (r *PiHookRunner) SimulatePiStop(sessionID, transcriptPath string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	}

	return r.runPiHookWithInput("stop", input)
}

// SimulatePiSessionEnd simulates the session-end hook for Pi.
func (r *PiHookRunner) SimulatePiSessionEnd(sessionID, transcriptPath string) error {
	r.T.Helper()

	input := map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	}

	return r.runPiHookWithInput("session-end", input)
}

// PiSession represents a simulated Pi session.
type PiSession struct {
	ID             string
	EntireID       string
	TranscriptPath string
	env            *TestEnv
}

// NewPiSession creates a new simulated Pi session.
func (env *TestEnv) NewPiSession() *PiSession {
	env.T.Helper()

	env.SessionCounter++
	sessionID := fmt.Sprintf("pi-session-%d", env.SessionCounter)
	entireID := sessionid.EntireSessionID(sessionID)
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", sessionID+".jsonl")

	return &PiSession{
		ID:             sessionID,
		EntireID:       entireID,
		TranscriptPath: transcriptPath,
		env:            env,
	}
}

// CreatePiTranscript creates a Pi JSONL transcript for this session.
func (s *PiSession) CreatePiTranscript(prompt string, changes []FileChange) string {
	if err := os.MkdirAll(filepath.Dir(s.TranscriptPath), 0o755); err != nil {
		s.env.T.Fatalf("failed to create transcript dir: %v", err)
	}

	entries := make([]map[string]interface{}, 0, len(changes)+3)
	entries = append(entries, map[string]interface{}{
		"type": "message",
		"id":   "1",
		"message": map[string]interface{}{
			"role":    "user",
			"content": prompt,
		},
	})

	assistantContent := make([]map[string]interface{}, 0, len(changes)+1)
	assistantContent = append(assistantContent, map[string]interface{}{
		"type": "text",
		"text": "I'll help with that.",
	})
	for i, change := range changes {
		assistantContent = append(assistantContent, map[string]interface{}{
			"type": "toolCall",
			"id":   fmt.Sprintf("tool-%d", i+1),
			"name": "write",
			"arguments": map[string]interface{}{
				"path": change.Path,
			},
		})
	}

	entries = append(entries, map[string]interface{}{
		"type": "message",
		"id":   "2",
		"message": map[string]interface{}{
			"role":    "assistant",
			"content": assistantContent,
		},
	})

	for i, change := range changes {
		entries = append(entries, map[string]interface{}{
			"type": "message",
			"id":   fmt.Sprintf("result-%d", i+1),
			"message": map[string]interface{}{
				"role":       "toolResult",
				"toolName":   "write",
				"toolCallId": fmt.Sprintf("tool-%d", i+1),
				"details": map[string]interface{}{
					"path": change.Path,
				},
			},
		})
	}

	var out bytes.Buffer
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			s.env.T.Fatalf("failed to marshal transcript entry: %v", err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}

	if err := os.WriteFile(s.TranscriptPath, out.Bytes(), 0o644); err != nil {
		s.env.T.Fatalf("failed to write transcript: %v", err)
	}

	return s.TranscriptPath
}

// SimulatePiSessionStart is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiSessionStart(sessionID string) error {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiSessionStart(sessionID)
}

// SimulatePiSessionStartWithOutput is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiSessionStartWithOutput(sessionID string) HookOutput {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiSessionStartWithOutput(sessionID)
}

// SimulatePiUserPromptSubmit is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiUserPromptSubmit(sessionID string) error {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiUserPromptSubmit(sessionID)
}

// SimulatePiBeforeTool is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiBeforeTool(sessionID, transcriptPath, toolName string, toolInput any) error {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiBeforeTool(sessionID, transcriptPath, toolName, toolInput)
}

// SimulatePiAfterTool is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiAfterTool(sessionID, transcriptPath, toolName string, toolInput any, toolResponse any) error {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiAfterTool(sessionID, transcriptPath, toolName, toolInput, toolResponse)
}

// SimulatePiStop is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiStop(sessionID, transcriptPath string) error {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiStop(sessionID, transcriptPath)
}

// SimulatePiSessionSwitch simulates Pi's session_switch lifecycle mapping:
// session-end(old) then session-start(new).
func (r *PiHookRunner) SimulatePiSessionSwitch(previousSessionID, newSessionID, previousTranscriptPath string) error {
	r.T.Helper()

	if err := r.SimulatePiSessionEnd(previousSessionID, previousTranscriptPath); err != nil {
		return fmt.Errorf("session-switch: session-end failed: %w", err)
	}
	if err := r.SimulatePiSessionStart(newSessionID); err != nil {
		return fmt.Errorf("session-switch: session-start failed: %w", err)
	}

	return nil
}

// SimulatePiSessionEnd is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiSessionEnd(sessionID, transcriptPath string) error {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiSessionEnd(sessionID, transcriptPath)
}

// SimulatePiSessionSwitch is a convenience method on TestEnv.
func (env *TestEnv) SimulatePiSessionSwitch(previousSessionID, newSessionID, previousTranscriptPath string) error {
	env.T.Helper()
	runner := NewPiHookRunner(env.RepoDir, env.T)
	return runner.SimulatePiSessionSwitch(previousSessionID, newSessionID, previousTranscriptPath)
}
