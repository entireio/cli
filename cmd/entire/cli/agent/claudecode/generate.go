package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GenerateText sends a prompt to the Claude CLI and returns the raw text response.
// Implements the agent.TextGenerator interface.
// The model parameter hints which model to use (e.g., "haiku", "sonnet").
// If empty, defaults to "haiku" for fast, cheap generation.
func (c *ClaudeCodeAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	claudePath := "claude"
	if model == "" {
		model = "haiku"
	}

	cmd := exec.CommandContext(ctx, claudePath,
		"--print", "--output-format", "json",
		"--model", model, "--setting-sources", "")

	// Isolate from the user's git repo to prevent recursive hook triggers
	// and index pollution (same approach as summarize/claude.go).
	cmd.Dir = os.TempDir()
	cmd.Env = stripGitEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "", fmt.Errorf("claude CLI not found: %w", err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("claude CLI failed (exit %d): %s", exitErr.ExitCode(), stderr.String())
		}
		return "", fmt.Errorf("failed to run claude CLI: %w", err)
	}

	// Parse the {"result": "..."} envelope
	var response struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return "", fmt.Errorf("failed to parse claude CLI response: %w", err)
	}

	return response.Result, nil
}

// stripGitEnv returns a copy of env with all GIT_* variables removed.
// This prevents a subprocess from discovering or modifying the parent's git repo.
// Duplicated from summarize/claude.go — simple filter not worth extracting to shared package.
func stripGitEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "GIT_") {
			filtered = append(filtered, e)
		}
	}
	return filtered
}
