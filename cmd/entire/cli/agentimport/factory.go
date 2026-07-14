package agentimport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// factoryImporter imports Factory AI Droid transcripts
// (~/.factory/sessions/<repo>/<id>.jsonl). Droid wraps each message in an
// envelope ({"type":"message","id":..,"message":{...}}); token usage is
// subagent-aware and the model lives in an adjacent <id>.settings.json.
type factoryImporter struct{}

func (factoryImporter) Name() string { return string(agent.AgentNameFactoryAIDroid) }

func (factoryImporter) AgentType() types.AgentType { return agent.AgentTypeFactoryAIDroid }

// Discover returns Factory transcript files for the repo modified within the
// lookback window.
func (factoryImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "factory", (&factoryaidroid.FactoryAIDroidAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	return discoverSessionFiles(dir, now, sessionFilter, jsonlSessionResolver(".jsonl", identitySessionID))
}

// SplitTurns produces one Turn per user-prompt envelope, bounded by the next.
// Token usage (including spawned subagents) is delegated to the Factory agent;
// the model is read once from the session's adjacent settings file. Droid
// envelopes carry no per-message timestamp (the agent stamps events with
// time.Now() at hook time), so every turn falls back to the transcript file's
// modtime — the same fallback the Gemini importer uses.
func (factoryImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	subagentsDir := filepath.Join(filepath.Dir(sf.Path), sf.SessionID, "subagents")
	model := factoryaidroid.ExtractModelFromTranscript(sf.Path)
	var createdAt time.Time
	if info, statErr := os.Stat(sf.Path); statErr == nil {
		createdAt = info.ModTime()
	}
	ag := &factoryaidroid.FactoryAIDroidAgent{}
	return splitLineTurns(splitRawLines(full),
		func(raw []byte) bool { _, ok := factoryPromptText(raw); return ok },
		func(rawLines [][]byte, start, _ int, truncated []byte) (*Turn, error) {
			tokens, err := ag.CalculateTotalTokenUsage(truncated, start, subagentsDir)
			if err != nil {
				return nil, fmt.Errorf("token usage: %w", err)
			}
			var env struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(rawLines[start], &env); err != nil {
				//nolint:nilerr // skip defensively; the line already parsed in factoryPromptText
				return nil, nil
			}
			prompt, _ := factoryPromptText(rawLines[start])
			return &Turn{UUID: env.ID, Prompt: prompt, Model: model, CreatedAt: createdAt, Tokens: tokens}, nil
		})
}

// factoryPromptText reports whether a raw Droid JSONL line is a user-prompt
// message and returns its text. Droid tags the role inside the inner message;
// tool_result user messages carry no extractable text and return false.
func factoryPromptText(raw []byte) (string, bool) {
	var env struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Type != "message" {
		return "", false
	}
	var role struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(env.Message, &role); err != nil || role.Role != transcript.TypeUser {
		return "", false
	}
	text := transcript.ExtractUserContent(env.Message)
	if text == "" {
		return "", false
	}
	return text, true
}
