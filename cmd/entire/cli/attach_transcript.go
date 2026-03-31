package cli

import (
	"encoding/json"

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
