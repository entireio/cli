package agentimport

import (
	"fmt"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// geminiImporter imports Gemini CLI transcripts
// (~/.gemini/tmp/<project-hash>/chats/session-*.json). Unlike the JSONL agents,
// a Gemini transcript is a single JSON document whose offsets are message
// indices, not line numbers, so import is per-session: one checkpoint covering
// the whole transcript rather than per-turn.
type geminiImporter struct{}

func (geminiImporter) Name() string { return string(agent.AgentNameGemini) }

func (geminiImporter) AgentType() types.AgentType { return agent.AgentTypeGemini }

// Discover returns Gemini transcript files for the repo modified within the
// lookback window. The session ID is the file stem (session-<date>-<shortid>).
func (geminiImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "gemini", (&geminicli.GeminiCLIAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	return discoverSessionFiles(dir, now, sessionFilter, jsonlSessionResolver(".json", identitySessionID))
}

// SplitTurns returns a single Turn covering the whole session. Offsets are
// message indices (the native Gemini space): LineStart 0, LineEnd the message
// count. Token usage is the whole-session total and the prompt is the first
// user message. The turn UUID is the session ID so re-imports stay idempotent.
func (geminiImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	tr, err := geminicli.ParseTranscript(full)
	if err != nil {
		return nil, fmt.Errorf("parse gemini transcript: %w", err)
	}
	if len(tr.Messages) == 0 {
		return nil, nil
	}

	ag := &geminicli.GeminiCLIAgent{}
	tokens, err := ag.CalculateTokenUsage(full, 0)
	if err != nil {
		return nil, fmt.Errorf("token usage: %w", err)
	}

	prompt := ""
	if prompts := geminicli.ExtractAllUserPromptsFromTranscript(tr); len(prompts) > 0 {
		prompt = prompts[0]
	}
	// Gemini messages carry no per-message timestamp; the file modtime is the
	// best available session time.
	var createdAt time.Time
	if info, statErr := os.Stat(sf.Path); statErr == nil {
		createdAt = info.ModTime()
	}

	return []Turn{{
		LineStart: 0,
		LineEnd:   len(tr.Messages),
		UUID:      sf.SessionID,
		Prompt:    prompt,
		CreatedAt: createdAt,
		Tokens:    tokens,
	}}, nil
}
