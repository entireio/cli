package geminicli

import "time"

// geminiStreamEvent is one decoded line of `gemini --output-format stream-json`
// NDJSON output. Fields are populated based on Type:
//
//   - "init":    SessionID, Model
//   - "message": Role ("user" or "assistant"), Content, Delta (true on incremental)
//   - "result":  Status ("success" | "error"), Error (when status=error), Stats
type geminiStreamEvent struct {
	Type      string             `json:"type"`
	Timestamp time.Time          `json:"timestamp,omitempty"`
	SessionID string             `json:"session_id,omitempty"`
	Model     string             `json:"model,omitempty"`
	Role      string             `json:"role,omitempty"`
	Content   string             `json:"content,omitempty"`
	Delta     bool               `json:"delta,omitempty"`
	Status    string             `json:"status,omitempty"`
	Error     *geminiStreamError `json:"error,omitempty"`
	Stats     *geminiStreamStats `json:"stats,omitempty"`
}

// geminiStreamError is the body of a result event with status="error".
type geminiStreamError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

// geminiStreamStats appears inside the terminal type=result event.
// Token field names mirror gemini-cli's stats schema.
type geminiStreamStats struct {
	TotalTokens  int `json:"total_tokens,omitempty"`
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	Cached       int `json:"cached,omitempty"`
	DurationMs   int `json:"duration_ms,omitempty"`
}
