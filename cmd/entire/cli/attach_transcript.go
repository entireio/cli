package cli

import (
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
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

	// Try JSONL format first (Claude Code, Cursor, OpenCode, etc.)
	lines, err := transcript.ParseFromBytes(data)
	if err == nil {
		for _, line := range lines {
			if line.Type == transcript.TypeUser {
				if prompt := transcript.ExtractUserContent(line.Message); prompt != "" {
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
		if meta.TurnCount > 0 || meta.Model != "" {
			return meta
		}
	}

	// Fallback: try Gemini JSON format {"messages": [...]}
	if prompts, gemErr := geminicli.ExtractAllUserPrompts(data); gemErr == nil && len(prompts) > 0 {
		meta.FirstPrompt = prompts[0]
		meta.TurnCount = len(prompts)
	}

	return meta
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
			meta.FirstPrompt = prompts[0]
			meta.TurnCount = len(prompts)
		}
	}
	if extractor, ok := agent.AsModelExtractor(ag); ok {
		if model, err := extractor.ExtractModel(data); err == nil && model != "" {
			meta.Model = model
		}
	}

	return meta
}
