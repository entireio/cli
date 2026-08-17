package cli

import (
	"encoding/json"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// transcriptMetadata holds metadata extracted from a single transcript parse pass.
type transcriptMetadata struct {
	FirstPrompt string
	TurnCount   int
	Model       string
}

// extractTranscriptMetadata parses transcript bytes once and extracts the first user prompt,
// user turn count, and model name. Supports both JSONL (Claude Code, Cursor, OpenCode) and
// Gemini JSON format.
func extractTranscriptMetadata(data []byte) transcriptMetadata {
	var meta transcriptMetadata

	// firstUserPrompt is the unfiltered first prompt, kept as a last-resort title
	// for a transcript whose every user message is agent-injected.
	var firstUserPrompt string

	// Try JSONL format first (Claude Code, Cursor, OpenCode, etc.)
	lines, err := transcript.ParseFromBytes(data)
	if err == nil {
		for _, line := range lines {
			if line.Type == transcript.TypeUser {
				if prompt := transcript.ExtractUserContent(line.Message); prompt != "" {
					if firstUserPrompt == "" {
						firstUserPrompt = prompt
					}
					// An injected preamble is not a user turn: counting it
					// inflates the step count attach reports.
					if textutil.IsInjectedPrompt(prompt) {
						continue
					}
					meta.TurnCount++
					if meta.FirstPrompt == "" {
						meta.FirstPrompt = prompt
					}
				}
			}
			if line.Type == transcript.TypeAssistant && meta.Model == "" {
				var msg struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(line.Message, &msg) == nil && msg.Model != "" {
					meta.Model = msg.Model
				}
			}
		}
		// A checkpoint with no prompt at all is worse than one titled with a
		// noisy-but-present preamble, and an empty FirstPrompt alongside a
		// non-zero TurnCount would also suppress warnEmptyTranscriptMetadata.
		if meta.FirstPrompt == "" {
			meta.FirstPrompt = firstUserPrompt
		}
		if meta.TurnCount > 0 || meta.Model != "" || meta.FirstPrompt != "" {
			return meta
		}
	}

	// Fallback: try Gemini JSON format {"messages": [...]}
	if prompts, gemErr := geminicli.ExtractAllUserPrompts(data); gemErr == nil && len(prompts) > 0 {
		if first := strategy.FirstDisplayPrompt(prompts); first != "" {
			meta.FirstPrompt = first
		} else {
			meta.FirstPrompt = prompts[0]
		}
		meta.TurnCount = countUserTurns(prompts)
	}

	return meta
}

// countUserTurns counts the prompts that represent an actual user turn, skipping
// agent-injected preambles and notifications. Once a prompt is deemed not to be
// user-authored it must not be counted as a user turn either, or attach reports
// more steps than the session had.
func countUserTurns(prompts []string) int {
	turns := 0
	for _, prompt := range prompts {
		if strings.TrimSpace(prompt) == "" || textutil.IsInjectedPrompt(prompt) {
			continue
		}
		turns++
	}
	return turns
}

// extractTranscriptMetadataForAgent augments the generic attach parser with
// agent-native prompt and model extraction when available. Native extractors
// are authoritative because they understand format-specific nesting and
// conversation branches (Pi, Codex, Droid, etc.); failures remain best-effort
// and preserve whatever the generic parser found.
func extractTranscriptMetadataForAgent(ag agent.Agent, sessionRef string, data []byte) transcriptMetadata {
	meta := extractTranscriptMetadata(data)

	if extractor, ok := agent.AsPromptExtractor(ag); ok {
		if prompts, err := extractor.ExtractPrompts(sessionRef, 0); err == nil && len(prompts) > 0 {
			// Native extractors return every user-role item in transcript order,
			// including the agent's own injected preambles (Codex leads with an
			// AGENTS.md dump and/or <environment_context>). Title from the first
			// genuine prompt, falling back to the raw first one so the checkpoint
			// is never left untitled.
			if first := strategy.FirstDisplayPrompt(prompts); first != "" {
				meta.FirstPrompt = first
			} else {
				meta.FirstPrompt = prompts[0]
			}
			meta.TurnCount = countUserTurns(prompts)
		}
	}
	if extractor, ok := agent.AsModelExtractor(ag); ok {
		if model, err := extractor.ExtractModel(data); err == nil && model != "" {
			meta.Model = model
		}
	}

	return meta
}
