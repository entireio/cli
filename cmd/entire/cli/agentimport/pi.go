package agentimport

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// piImporter imports Pi transcripts (~/.pi/agent/sessions/<repo>/<ts>_<uuid>.jsonl).
// Pi records token usage and the model on every assistant message, so imported
// turns carry both via the agent's own CalculateTokenUsage / ExtractModel.
type piImporter struct{}

func (piImporter) Name() string { return string(agent.AgentNamePi) }

func (piImporter) AgentType() types.AgentType { return agent.AgentTypePi }

// Discover returns Pi transcript files for the repo modified within the lookback
// window. The session ID is the <uuid> suffix of the <timestamp>_<uuid> file
// stem (Pi timestamps use dashes, so the first underscore is the separator).
func (piImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "pi", (&pi.PiAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	return discoverSessionFiles(dir, now, sessionFilter, jsonlSessionResolver(".jsonl", piSessionID))
}

// piSessionID extracts the <uuid> portion of a "<timestamp>_<uuid>" file stem.
// Falls back to the whole stem when there is no underscore separator.
func piSessionID(stem string) string {
	if i := strings.Index(stem, "_"); i >= 0 {
		return stem[i+1:]
	}
	return stem
}

// SplitTurns produces one Turn per user-prompt message line, bounded by the
// next. Token usage and model are delegated to the Pi agent so import reuses the
// same accounting (branch-aware) the live path uses.
func (piImporter) SplitTurns(_ SessionFile, full []byte) ([]Turn, error) {
	ag := &pi.PiAgent{}
	return splitLineTurns(splitRawLines(full),
		func(raw []byte) bool { _, ok := piPromptText(raw); return ok },
		func(rawLines [][]byte, start, _ int, truncated []byte) (*Turn, error) {
			// truncated is the [0,end) prefix (file kept from line 0). Pi's
			// branch-aware helpers walk parentId back to the root, so the prefix
			// MUST retain the beginning — truncating the end is safe (parents are
			// earlier lines) but slicing off the start would break those chains.
			// CalculateTokenUsage slices forward from `start`; ExtractModel reports
			// the active-branch model as of this turn's end.
			tokens, err := ag.CalculateTokenUsage(truncated, start)
			if err != nil {
				return nil, fmt.Errorf("token usage: %w", err)
			}
			model, mErr := ag.ExtractModel(truncated)
			if mErr != nil {
				model = ""
			}
			var entry pijsonl.Entry
			if err := json.Unmarshal(rawLines[start], &entry); err != nil {
				//nolint:nilerr // skip defensively; the line already parsed in piPromptText
				return nil, nil
			}
			prompt, _ := piPromptText(rawLines[start])
			return &Turn{UUID: entry.ID, Prompt: prompt, Model: model, CreatedAt: parseTimestamp(entry.Timestamp), Tokens: tokens}, nil
		})
}

// piPromptText reports whether a raw Pi JSONL line is a user-prompt message and
// returns its text. Pi user content may be a plain string or an array of typed
// blocks; toolResult/assistant messages and empty content return false.
func piPromptText(raw []byte) (string, bool) {
	var entry pijsonl.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", false
	}
	if entry.Type != pijsonl.EntryTypeMessage || entry.Message.Role != pijsonl.RoleUser {
		return "", false
	}
	if s := pijsonl.DecodeStringContent(entry.Message.Content); s != "" {
		return s, true
	}
	var items []pijsonl.ContentItem
	if err := json.Unmarshal(entry.Message.Content, &items); err != nil {
		return "", false
	}
	var texts []string
	for _, it := range items {
		if it.Type == pijsonl.ContentTypeText && it.Text != "" {
			texts = append(texts, it.Text)
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, "\n\n"), true
}
