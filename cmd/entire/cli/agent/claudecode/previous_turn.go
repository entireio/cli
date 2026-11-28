package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	transcriptlib "github.com/entireio/cli/transcript"
)

var _ agent.PreviousTurnStartReader = (*ClaudeCodeAgent)(nil)

// PreviousTurnStart implements agent.PreviousTurnStartReader: the timestamp of
// the latest user prompt that the assistant answered. Claude Code may or may
// not have flushed the prompt now being submitted when UserPromptSubmit fires;
// an unanswered trailing prompt is that one, so it does not count.
func (c *ClaudeCodeAgent) PreviousTurnStart(sessionRef string) (time.Time, bool, error) {
	data, err := c.ReadTranscript(sessionRef)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	return previousTurnStart(data)
}

func previousTurnStart(data []byte) (time.Time, bool, error) {
	var answered, pending string // timestamps of the latest answered and unanswered prompts
	for _, raw := range bytes.Split(data, []byte("\n")) {
		var header struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(bytes.TrimSpace(raw), &header) != nil {
			continue
		}
		if header.Type == transcriptlib.TypeAssistant && pending != "" {
			answered, pending = pending, ""
			continue
		}
		if _, ok := userPrompt(raw); ok {
			pending = header.Timestamp
		}
	}
	if answered == "" {
		return time.Time{}, false, nil
	}
	start, err := time.Parse(time.RFC3339Nano, answered)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("claude-code: parse prompt timestamp: %w", err)
	}
	return start, true, nil
}
