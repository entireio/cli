package cursor

// cursorStreamEvent is one decoded line of `agent --print --output-format
// stream-json --stream-partial-output` NDJSON output. Fields are populated
// based on Type.
//
// Real-CLI shape (Cursor agent, Composer 2.5 Fast):
//   - system,subtype:init                          → session bootstrap (model, cwd)
//   - user.message                                 → echoed input, ignored
//   - thinking,subtype:delta / completed           → internal reasoning, ignored
//   - assistant (with timestamp_ms) delta          → incremental text token
//   - assistant (no timestamp_ms) aggregated       → final consolidated message,
//     ignored (use result.result)
//   - result,subtype:success                       → terminal: usage + duration
//   - result,is_error:true                         → terminal error envelope
type cursorStreamEvent struct {
	Type        string               `json:"type"`
	Subtype     string               `json:"subtype,omitempty"`
	IsError     bool                 `json:"is_error,omitempty"`
	Result      string               `json:"result,omitempty"`
	DurationMs  int                  `json:"duration_ms,omitempty"`
	TimestampMs int64                `json:"timestamp_ms,omitempty"`
	Message     *cursorStreamMessage `json:"message,omitempty"`
	Usage       *cursorStreamUsage   `json:"usage,omitempty"`
}

// cursorStreamMessage is the assistant/user message payload.
type cursorStreamMessage struct {
	Role    string                       `json:"role,omitempty"`
	Content []cursorStreamMessageContent `json:"content,omitempty"`
}

// cursorStreamMessageContent is one content block within a message.
type cursorStreamMessageContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// cursorStreamUsage appears inside the terminal type=result event. Cursor
// reports input, output, and cache-read token counts.
type cursorStreamUsage struct {
	InputTokens      int `json:"inputTokens,omitempty"`
	OutputTokens     int `json:"outputTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}
