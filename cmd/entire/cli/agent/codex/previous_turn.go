package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.PreviousTurnStartReader = (*CodexAgent)(nil)

// PreviousTurnStart implements agent.PreviousTurnStartReader: the timestamp of
// the latest task_started event whose turn produced an assistant message.
// Codex writes the starting turn's task_started before UserPromptSubmit fires;
// an unanswered trailing turn is that one, so it does not count.
func (c *CodexAgent) PreviousTurnStart(sessionRef string) (time.Time, bool, error) {
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
	var answered, pending string // timestamps of the latest answered and unanswered turn starts
	for _, lineData := range splitJSONL(data) {
		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil {
			continue
		}
		switch line.Type {
		case rolloutLineTypeEventMsg:
			var evt eventMsgPayload
			if json.Unmarshal(line.Payload, &evt) == nil && evt.Type == eventMsgTypeTaskStarted {
				pending = line.Timestamp
			}
		case rolloutLineTypeResponseItem:
			var item responseItemPayload
			if pending != "" && json.Unmarshal(line.Payload, &item) == nil &&
				item.Type == itemTypeMessage && item.Role == "assistant" {
				answered, pending = pending, ""
			}
		}
	}
	if answered == "" {
		return time.Time{}, false, nil
	}
	start, err := time.Parse(time.RFC3339Nano, answered)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("codex: parse task_started timestamp: %w", err)
	}
	return start, true, nil
}
