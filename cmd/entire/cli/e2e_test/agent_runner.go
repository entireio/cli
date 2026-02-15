//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// AgentNameClaudeCode is the name for Claude Code agent.
const AgentNameClaudeCode = "claude-code"

// AgentNameGeminiCLI is the name for Gemini CLI agent.
const AgentNameGeminiCLI = "gemini-cli"

// AgentRunner abstracts invoking a coding agent for e2e tests.
// This follows the multi-agent pattern from cmd/entire/cli/agent/agent.go.
type AgentRunner interface {
	// Name returns the agent name (e.g., "claude-code", "gemini-cli")
	Name() string

	// IsAvailable checks if the agent CLI is installed and authenticated
	IsAvailable() (bool, error)

	// RunPrompt executes a prompt and returns the result
	RunPrompt(ctx context.Context, workDir string, prompt string) (*AgentResult, error)

	// RunPromptWithTools executes with specific allowed tools
	RunPromptWithTools(ctx context.Context, workDir string, prompt string, tools []string) (*AgentResult, error)
}

// AgentResult holds the result of an agent invocation.
type AgentResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// AgentRunnerConfig holds configuration for agent runners.
type AgentRunnerConfig struct {
	Model   string        // Model to use (e.g., "haiku" for Claude)
	Timeout time.Duration // Timeout per prompt
}

// NewAgentRunner creates an agent runner based on the agent name.
//
//nolint:ireturn // factory pattern intentionally returns interface
func NewAgentRunner(name string, config AgentRunnerConfig) AgentRunner {
	switch name {
	case AgentNameClaudeCode:
		return NewClaudeCodeRunner(config)
	case AgentNameGeminiCLI:
		return NewGeminiCLIRunner(config)
	default:
		// Return a runner that reports as unavailable
		return &unavailableRunner{name: name}
	}
}

// unavailableRunner is returned for unknown agent names.
type unavailableRunner struct {
	name string
}

func (r *unavailableRunner) Name() string { return r.name }

func (r *unavailableRunner) IsAvailable() (bool, error) {
	return false, fmt.Errorf("unknown agent: %s", r.name)
}

func (r *unavailableRunner) RunPrompt(_ context.Context, _ string, _ string) (*AgentResult, error) {
	return nil, fmt.Errorf("agent %s is not available", r.name)
}

func (r *unavailableRunner) RunPromptWithTools(_ context.Context, _ string, _ string, _ []string) (*AgentResult, error) {
	return nil, fmt.Errorf("agent %s is not available", r.name)
}

// ClaudeCodeRunner implements AgentRunner for Claude Code CLI.
type ClaudeCodeRunner struct {
	Model        string
	Timeout      time.Duration
	AllowedTools []string
}

// NewClaudeCodeRunner creates a new Claude Code runner with the given config.
func NewClaudeCodeRunner(config AgentRunnerConfig) *ClaudeCodeRunner {
	model := config.Model
	if model == "" {
		model = os.Getenv("E2E_CLAUDE_MODEL")
		if model == "" {
			model = "haiku"
		}
	}

	timeout := config.Timeout
	if timeout == 0 {
		if envTimeout := os.Getenv("E2E_TIMEOUT"); envTimeout != "" {
			if parsed, err := time.ParseDuration(envTimeout); err == nil {
				timeout = parsed
			}
		}
		if timeout == 0 {
			timeout = 2 * time.Minute
		}
	}

	return &ClaudeCodeRunner{
		Model:        model,
		Timeout:      timeout,
		AllowedTools: []string{"Edit", "Read", "Write", "Bash", "Glob", "Grep"},
	}
}

func (r *ClaudeCodeRunner) Name() string {
	return AgentNameClaudeCode
}

// IsAvailable checks if Claude CLI is installed and responds to --version.
// Note: This does NOT verify authentication status. Claude Code uses OAuth
// authentication (via `claude login`), not ANTHROPIC_API_KEY. If the CLI is
// installed but not logged in, tests will fail at RunPrompt time.
func (r *ClaudeCodeRunner) IsAvailable() (bool, error) {
	// Check if claude CLI is in PATH
	if _, err := exec.LookPath("claude"); err != nil {
		return false, fmt.Errorf("claude CLI not found in PATH: %w", err)
	}

	// Check if claude is working (--version doesn't require auth)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "--version")
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("claude CLI not working: %w", err)
	}

	return true, nil
}

func (r *ClaudeCodeRunner) RunPrompt(ctx context.Context, workDir string, prompt string) (*AgentResult, error) {
	return r.RunPromptWithTools(ctx, workDir, prompt, r.AllowedTools)
}

func (r *ClaudeCodeRunner) RunPromptWithTools(ctx context.Context, workDir string, prompt string, tools []string) (*AgentResult, error) {
	// Build command: claude --model <model> -p "<prompt>" --allowedTools <tools>
	args := []string{
		"--model", r.Model,
		"-p", prompt,
	}

	if len(tools) > 0 {
		// Claude CLI expects each tool as a separate argument after --allowedTools
		// e.g., --allowedTools Edit Read Bash (not --allowedTools "Edit,Read,Bash")
		args = append(args, "--allowedTools")
		args = append(args, tools...)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	//nolint:gosec // args are constructed from trusted config, not user input
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	result := &AgentResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
		//nolint:wrapcheck // error is from exec.Run, caller can check ExitCode in result
		return result, err
	}

	result.ExitCode = 0
	return result, nil
}

// GeminiCLIRunner implements AgentRunner for Gemini CLI.
// This is a placeholder for future implementation.
type GeminiCLIRunner struct {
	Timeout time.Duration
}

// NewGeminiCLIRunner creates a new Gemini CLI runner with the given config.
func NewGeminiCLIRunner(config AgentRunnerConfig) *GeminiCLIRunner {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}

	return &GeminiCLIRunner{
		Timeout: timeout,
	}
}

func (r *GeminiCLIRunner) Name() string {
	return AgentNameGeminiCLI
}

// IsAvailable checks if Gemini CLI is installed and authenticated.
func (r *GeminiCLIRunner) IsAvailable() (bool, error) {
	// Check if gemini CLI is in PATH
	if _, err := exec.LookPath("gemini"); err != nil {
		return false, fmt.Errorf("gemini CLI not found in PATH: %w", err)
	}

	// Check if gemini is working
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gemini", "--version")
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("gemini CLI not working: %w", err)
	}

	return true, nil
}

func (r *GeminiCLIRunner) RunPrompt(ctx context.Context, workDir string, prompt string) (*AgentResult, error) {
	return r.RunPromptWithTools(ctx, workDir, prompt, nil)
}

func (r *GeminiCLIRunner) RunPromptWithTools(_ context.Context, _ string, _ string, _ []string) (*AgentResult, error) {
	// Gemini CLI implementation would go here
	// For now, return an error indicating it's not fully implemented
	return nil, errors.New("gemini CLI runner not yet implemented")
}
