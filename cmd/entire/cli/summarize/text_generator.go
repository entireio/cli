package summarize

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

// TextGeneratorAdapter uses an agent.TextGenerator with Entire's shared
// summary prompt and response parser. Detects streaming-capable agents
// (StreamingTextGenerator) and prefers them when present so the explain
// progress UI receives live phase events.
type TextGeneratorAdapter struct {
	TextGenerator agent.TextGenerator
	Model         string

	// progress is forwarded by GenerateFromTranscript when the caller passes
	// a non-nil ProgressFn. Used only by streaming-capable underlying
	// generators; non-streaming agents leave this unused. Unexported so
	// external packages cannot bypass GenerateFromTranscript and set it
	// directly.
	progress agent.ProgressFn
}

// Generate creates a summary using the shared prompt, then delegates raw text
// generation to the configured agent provider. Prefers
// agent.StreamingTextGenerator when the underlying agent supports it (so
// progress events reach the explain UI); falls through to GenerateText
// otherwise.
func (g *TextGeneratorAdapter) Generate(ctx context.Context, input Input) (*checkpoint.Summary, error) {
	if g.TextGenerator == nil {
		return nil, errors.New("text generator not configured")
	}
	transcriptText := FormatCondensedTranscript(input)
	prompt := buildSummarizationPrompt(transcriptText)

	// Prefer streaming when the underlying agent supports it. TextGenerator
	// embeds Agent, so AsStreamingTextGenerator accepts it directly.
	if streamer, ok := agent.AsStreamingTextGenerator(g.TextGenerator); ok {
		result, err := streamer.GenerateTextStreaming(ctx, prompt, g.Model, g.progress)
		if err != nil {
			return nil, err //nolint:wrapcheck // preserve *agent.TextGenerationError / *claudecode.ClaudeError for errors.As
		}
		return parseSummaryText(result)
	}

	result, err := g.TextGenerator.GenerateText(ctx, prompt, g.Model)
	if err != nil {
		return nil, fmt.Errorf("provider text generation failed: %w", err)
	}

	return parseSummaryText(result)
}
