package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
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

var defaultTextGeneratorFactory = func() (agent.TextGenerator, error) {
	textGenerator, ok := agent.AsTextGenerator(claudecode.NewClaudeCodeAgent())
	if !ok {
		return nil, errors.New("default summarizer does not support text generation")
	}
	return textGenerator, nil
}

// ClaudeGenerator generates summaries using the Claude CLI.
type ClaudeGenerator struct {
	// TextGenerator is the primitive used to obtain raw model output.
	// If nil, uses the built-in Claude Code text generator.
	TextGenerator agent.TextGenerator

	// Model is the Claude model to use for summarization.
	// If empty, defaults to DefaultModel ("sonnet").
	Model string
}

// Generate creates a summary from checkpoint data by calling the Claude CLI.
func (g *ClaudeGenerator) Generate(ctx context.Context, input Input) (*checkpoint.Summary, error) {
	// Format the transcript for the prompt
	transcriptText := FormatCondensedTranscript(input)

	// Build the prompt
	prompt := buildSummarizationPrompt(transcriptText)

	model := g.Model
	if model == "" {
		model = DefaultModel
	}

	textGenerator := g.TextGenerator
	if textGenerator == nil {
		var err error
		textGenerator, err = defaultTextGeneratorFactory()
		if err != nil {
			return nil, err
		}
	}

	resultJSON, err := textGenerator.GenerateText(ctx, prompt, model)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve *ClaudeError for errors.As at the explain layer
	}

	return parseSummaryText(resultJSON)
}

// buildSummarizationPrompt creates the prompt for the Claude CLI.
func buildSummarizationPrompt(transcriptText string) string {
	return fmt.Sprintf(summarizationPromptTemplate, transcriptText)
}

func parseSummaryText(result string) (*checkpoint.Summary, error) {
	resultJSON := extractJSONFromMarkdown(result)

	var summary checkpoint.Summary
	if err := json.Unmarshal([]byte(resultJSON), &summary); err != nil {
		start := strings.Index(resultJSON, "{")
		end := strings.LastIndex(resultJSON, "}")
		if start >= 0 && end > start {
			candidate := resultJSON[start : end+1]
			if candidate != resultJSON {
				if retryErr := json.Unmarshal([]byte(candidate), &summary); retryErr == nil {
					return &summary, nil
				}
			}
		}
		return nil, fmt.Errorf("failed to parse summary JSON: %w (response: %s)", err, resultJSON)
	}

	return &summary, nil
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
