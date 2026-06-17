package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
)

type responseEnvelope struct {
	Type           string  `json:"type"`
	Subtype        string  `json:"subtype"`
	IsError        bool    `json:"is_error"`
	APIErrorStatus *int    `json:"api_error_status"`
	Result         *string `json:"result"`
}

// parseGenerateTextResponse extracts the raw text payload and envelope metadata
// from Claude CLI JSON output. Claude may return either a single object or an array of events.
// The returned envelope allows callers to check IsError and APIErrorStatus.
func parseGenerateTextResponse(stdout []byte) (string, *responseEnvelope, error) {
	var response responseEnvelope
	if err := json.Unmarshal(stdout, &response); err == nil {
		if response.Result != nil {
			return *response.Result, &response, nil
		}
		// is_error:true with null result is a structured CLI failure; callers
		// need the envelope (IsError, APIErrorStatus) for classification.
		if response.IsError {
			return "", &response, nil
		}
	}

	var responses []responseEnvelope
	if err := json.Unmarshal(stdout, &responses); err != nil {
		return "", nil, fmt.Errorf("unsupported Claude CLI JSON response: %w", err)
	}

	for i := len(responses) - 1; i >= 0; i-- {
		if responses[i].Type != "result" {
			continue
		}
		if responses[i].Result != nil {
			return *responses[i].Result, &responses[i], nil
		}
		// Mirror the object-path behavior: is_error:true with null result is
		// a structured failure whose envelope (IsError, APIErrorStatus) must
		// reach classifyEnvelopeError.
		if responses[i].IsError {
			return "", &responses[i], nil
		}
	}

	return "", nil, errors.New("unsupported Claude CLI JSON response: missing result item")
}
