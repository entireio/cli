package agentimport

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// claudeImporter imports Claude Code transcripts (~/.claude/projects/<slug>/*.jsonl).
type claudeImporter struct{}

func (claudeImporter) Name() string { return "claude-code" }

func (claudeImporter) AgentType() types.AgentType { return agent.AgentTypeClaudeCode }

// Discover returns Claude transcript files for the repo modified within the
// lookback window. overridePath replaces the default ~/.claude/projects/<slug>
// dir; sessionFilter, when non-empty, keeps only matching session IDs (the
// file stem).
func (claudeImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "claude", (&claudecode.ClaudeCodeAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	return discoverSessionFiles(dir, now, sessionFilter, jsonlSessionResolver(".jsonl", identitySessionID))
}

// SplitTurns produces one Turn per user-prompt line. Token usage for each turn
// is computed on the slice [LineStart, LineEnd) so turns don't double-count
// later turns. tool_result lines (Type == "user" but no text content) do not
// start a turn.
func (claudeImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	subagentsDir := filepath.Join(filepath.Dir(sf.Path), sf.SessionID, "subagents")
	ag := &claudecode.ClaudeCodeAgent{}
	return splitLineTurns(splitRawLines(full), isUserPromptLine,
		func(rawLines [][]byte, start, end int, truncated []byte) (*Turn, error) {
			tokens, err := ag.CalculateTotalTokenUsage(truncated, start, subagentsDir)
			if err != nil {
				return nil, fmt.Errorf("token usage: %w", err)
			}
			var rec struct {
				UUID      string          `json:"uuid"`
				Message   json.RawMessage `json:"message"`
				Timestamp string          `json:"timestamp"`
			}
			if err := json.Unmarshal(rawLines[start], &rec); err != nil {
				//nolint:nilerr // skip defensively; the line already parsed in isUserPromptLine
				return nil, nil
			}
			return &Turn{
				UUID:      rec.UUID,
				Prompt:    transcript.ExtractUserContent(rec.Message),
				Model:     modelInRange(rawLines, start, end),
				CreatedAt: parseTimestamp(rec.Timestamp),
				Tokens:    tokens,
			}, nil
		})
}

// claudeExtraFields are line fields not modeled by transcript.Line.
type claudeExtraFields struct {
	ParentUUID string `json:"parentUuid"`
	Timestamp  string `json:"timestamp"`
	Message    struct {
		Model string `json:"model"`
	} `json:"message"`
}

// modelInRange returns the model from the first assistant message within
// [start, end), or "" when none carries one. The model lives on assistant
// lines, not the user-prompt line.
func modelInRange(rawLines [][]byte, start, end int) string {
	for i := start; i < end && i < len(rawLines); i++ {
		var line transcript.Line
		if err := json.Unmarshal(rawLines[i], &line); err != nil {
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		var ex claudeExtraFields
		if err := json.Unmarshal(rawLines[i], &ex); err != nil {
			continue
		}
		if ex.Message.Model != "" {
			return ex.Message.Model
		}
	}
	return ""
}

// isUserPromptLine reports whether a raw JSONL line is a genuine user-prompt
// turn start: type "user" (or role "user") with non-empty extractable text.
// tool_result lines are type "user" but carry no text, so they return false.
func isUserPromptLine(raw []byte) bool {
	var line transcript.Line
	if err := json.Unmarshal(raw, &line); err != nil {
		return false
	}
	typ := line.Type
	if typ == "" {
		typ = line.Role
	}
	if typ != "user" {
		return false
	}
	return transcript.ExtractUserContent(line.Message) != ""
}

// splitRawLines splits content into raw lines in the same index space as
// transcript.SliceFromLine (newline-counted). Trailing empty segment from a
// final newline is dropped.
func splitRawLines(content []byte) [][]byte {
	if len(content) == 0 {
		return nil
	}
	parts := strings.Split(string(content), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	out := make([][]byte, len(parts))
	for i, p := range parts {
		out[i] = []byte(p)
	}
	return out
}

// joinLines reassembles raw lines into newline-terminated bytes.
func joinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	strs := make([]string, len(lines))
	for i, l := range lines {
		strs[i] = string(l)
	}
	return []byte(strings.Join(strs, "\n") + "\n")
}
