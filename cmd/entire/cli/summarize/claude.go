package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"entire.io/cli/cmd/entire/cli/checkpoint"
)

// summarizationPromptTemplate is the prompt used to generate summaries via the Claude CLI.
//
// Security note: The transcript is wrapped in <transcript> tags to provide clear boundary
// markers. This helps contain any potentially malicious content within the transcript
// (e.g., prompt injection attempts in user messages or file contents) by giving the LLM
// a clear structural signal about where the untrusted content begins and ends.
const summarizationPromptTemplate = `Analyze this development session transcript and generate a structured summary.

<transcript>
%s
</transcript>

Return a JSON object with this exact structure:
{
  "intent": "What the user was trying to accomplish (1-2 sentences)",
  "outcome": "What was actually achieved (1-2 sentences)",
  "learnings": {
    "repo": ["Codebase-specific patterns, conventions, or gotchas discovered"],
    "code": [{"path": "file/path.go", "line": 42, "end_line": 56, "finding": "What was learned"}],
    "workflow": ["General development practices or tool usage insights"]
  },
  "friction": ["Problems, blockers, or annoyances encountered"],
  "open_items": ["Tech debt, unfinished work, or things to revisit later"]
}

Guidelines:
- Be concise but specific
- Include line numbers for code learnings when the transcript references specific lines
- Friction should capture both blockers and minor annoyances
- Open items are things intentionally deferred, not failures
- Empty arrays are fine if a category doesn't apply
- Return ONLY the JSON object, no markdown formatting or explanation`

// DefaultModel is the default model used for summarization.
// Sonnet provides a good balance of quality and cost, with 1M context window
// to handle long transcripts without truncation.
const DefaultModel = "sonnet"

// ClaudeGenerator generates summaries using the Claude CLI.
type ClaudeGenerator struct {
	// ClaudePath is the path to the claude CLI executable.
	// If empty, defaults to "claude" (expects it to be in PATH).
	ClaudePath string

	// Model is the Claude model to use for summarization.
	// If empty, defaults to DefaultModel ("sonnet").
	Model string

	// CommandRunner allows injection of the command execution for testing.
	// If nil, uses exec.CommandContext directly.
	CommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// claudeCLIResponse represents the JSON response from the Claude CLI.
type claudeCLIResponse struct {
	Result string `json:"result"`
}

// Generate creates a summary from checkpoint data by calling the Claude CLI.
func (g *ClaudeGenerator) Generate(ctx context.Context, input Input) (*checkpoint.Summary, error) {
	// Format the transcript for the prompt
	transcriptText := FormatCondensedTranscript(input)

	// Build the prompt
	prompt := buildSummarizationPrompt(transcriptText)

	// Execute the Claude CLI
	runner := g.CommandRunner
	if runner == nil {
		runner = exec.CommandContext
	}

	claudePath := g.ClaudePath
	if claudePath == "" {
		claudePath = "claude"
	}

	model := g.Model
	if model == "" {
		model = DefaultModel
	}

	// Use --setting-sources user to avoid project hooks interfering with --print mode
	cmd := runner(ctx, claudePath, "--print", "--output-format", "json", "--model", model, "--setting-sources", "user")

	// Pass prompt via stdin
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// Check if the command was not found
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, fmt.Errorf("claude CLI not found: %w", err)
		}

		// Check for exit error
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("claude CLI failed (exit %d): %s", exitErr.ExitCode(), stderr.String())
		}

		return nil, fmt.Errorf("failed to run claude CLI: %w", err)
	}

	// Parse the CLI response
	var cliResponse claudeCLIResponse
	if err := json.Unmarshal(stdout.Bytes(), &cliResponse); err != nil {
		return nil, fmt.Errorf("failed to parse claude CLI response: %w", err)
	}

	// The result field contains the actual JSON summary
	resultJSON := cliResponse.Result

	// Try to extract JSON if it's wrapped in markdown code blocks
	resultJSON = extractJSONFromMarkdown(resultJSON)

	// Parse the summary from the result
	var summary checkpoint.Summary
	if err := json.Unmarshal([]byte(resultJSON), &summary); err != nil {
		return nil, fmt.Errorf("failed to parse summary JSON: %w (response: %s)", err, resultJSON)
	}

	return &summary, nil
}

// buildSummarizationPrompt creates the prompt for the Claude CLI.
func buildSummarizationPrompt(transcriptText string) string {
	return fmt.Sprintf(summarizationPromptTemplate, transcriptText)
}

// extractJSONFromMarkdown attempts to extract JSON from markdown code blocks.
// If the input is not wrapped in code blocks, it returns the input unchanged.
func extractJSONFromMarkdown(s string) string {
	s = strings.TrimSpace(s)

	// Check for ```json ... ``` blocks
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		return strings.TrimSpace(s)
	}

	// Check for ``` ... ``` blocks
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		return strings.TrimSpace(s)
	}

	return s
}
