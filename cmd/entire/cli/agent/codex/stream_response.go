package codex

// codexStreamEvent is one decoded line of `codex exec --json` output.
// Fields are populated based on the event Type/Item.Type.
type codexStreamEvent struct {
	Type     string            `json:"type"`
	ThreadID string            `json:"thread_id,omitempty"`
	Item     *codexStreamItem  `json:"item,omitempty"`
	Usage    *codexStreamUsage `json:"usage,omitempty"`
}

// codexStreamItem appears inside type=item.completed events. For summary
// generation we care only about Type="agent_message" items, which carry
// the model's response in Text.
type codexStreamItem struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "agent_message" | "command_execution" | ...
	Text string `json:"text"` // populated for agent_message
}

// codexStreamUsage appears inside the terminal type=turn.completed event.
type codexStreamUsage struct {
	InputTokens           int `json:"input_tokens,omitempty"`
	CachedInputTokens     int `json:"cached_input_tokens,omitempty"`
	OutputTokens          int `json:"output_tokens,omitempty"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens,omitempty"`
}
