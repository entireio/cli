package summarize

import (
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// ResolveGenerator creates a Generator based on the summarize provider settings.
// Falls back to ClaudeGenerator when no provider is configured or settings is nil.
func ResolveGenerator(s *settings.EntireSettings) Generator { //nolint:ireturn // factory returns interface by design
	if s == nil {
		return &ClaudeGenerator{}
	}
	model := s.SummarizeModel()
	switch s.SummarizeProvider() {
	case "openai":
		return &OpenAIGenerator{APIKey: s.SummarizeAPIKey(), Model: model}
	default:
		return &ClaudeGenerator{Model: model}
	}
}

// ScopeTranscript slices a transcript to start from the given offset.
// For Gemini (JSON), the offset is a message index; for Claude (JSONL), it is a line offset.
func ScopeTranscript(transcriptBytes []byte, startOffset int, agentType agent.AgentType) []byte {
	if agentType == agent.AgentTypeGemini {
		return geminicli.SliceFromMessage(transcriptBytes, startOffset)
	}
	return transcript.SliceFromLine(transcriptBytes, startOffset)
}
