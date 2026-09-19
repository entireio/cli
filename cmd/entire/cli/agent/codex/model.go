package codex

import (
	"bytes"
	"encoding/json"
)

// ExtractModel returns the latest non-empty turn_context model, matching
// mid-session model changes. Missing or malformed records (including a partially
// flushed tail) do not erase the last known model.
func (a *CodexAgent) ExtractModel(transcriptData []byte) (string, error) {
	var model string
	for raw := range bytes.SplitSeq(transcriptData, []byte("\n")) {
		var line struct {
			Type    string `json:"type"`
			Payload struct {
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &line) == nil && line.Type == "turn_context" && line.Payload.Model != "" {
			model = line.Payload.Model
		}
	}
	return model, nil
}
