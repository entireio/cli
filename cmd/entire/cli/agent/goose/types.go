package goose

import "encoding/json"

// ExportSession is Goose's `goose session export --format json` envelope.
//
// Field names were captured from goose v1.46.0 by round-tripping a transcript
// through an isolated XDG_DATA_HOME; see AGENT.md. Two spellings are easy to get
// wrong and are load-bearing here:
//
//   - the message array is "conversation", not "messages" (OpenCode's analogous
//     export uses "messages", Goose does not);
//   - the cache token fields are "cache_read_input_tokens" /
//     "cache_write_input_tokens" in the export, even though the SQLite columns
//     behind them are named cache_read_tokens / cache_write_tokens.
//
// This struct is used for *reading* (token accounting, file extraction, prompt
// extraction). It deliberately does not describe all 24 top-level keys, so it
// must never be used to re-serialize a session: chunking and reassembly go
// through splitExport/withConversation in transcript.go, which operate on
// a raw field map and therefore preserve keys this struct does not name.
type ExportSession struct {
	ID           string          `json:"id"`
	WorkingDir   string          `json:"working_dir,omitempty"`
	Name         string          `json:"name,omitempty"`
	Conversation []ExportMessage `json:"conversation"`
	Usage        *ExportUsage    `json:"usage,omitempty"`
	Accumulated  *ExportUsage    `json:"accumulated_usage,omitempty"`
	ProviderName string          `json:"provider_name,omitempty"`
	ModelConfig  *ExportModelCfg `json:"model_config,omitempty"`
	MessageCount int             `json:"message_count,omitempty"`
}

// ExportUsage is the token accounting block Goose attaches to an export.
type ExportUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	CacheWriteTokens int `json:"cache_write_input_tokens"`
}

// ExportModelCfg carries the model identity for a session. Goose nests the model
// name here rather than putting it on each message.
type ExportModelCfg struct {
	Model string `json:"model,omitempty"`
}

// ExportMessage is one entry in the conversation array.
type ExportMessage struct {
	ID       string          `json:"id"`
	Role     string          `json:"role"`
	Created  int64           `json:"created,omitempty"`
	Content  []ContentBlock  `json:"content"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// ContentBlock is a single block inside a message. Goose ships four types:
// text, toolRequest, toolResponse and toolConfirmationRequest.
type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ID       string    `json:"id,omitempty"`
	ToolCall *ToolCall `json:"toolCall,omitempty"`
}

// ToolCall wraps a tool invocation. Goose nests the useful part one level deeper
// than most agents: the tool name and arguments live under toolCall.value.
type ToolCall struct {
	Status string        `json:"status,omitempty"`
	Value  *ToolCallData `json:"value,omitempty"`
}

// ToolCallData is the tool name plus its arguments.
type ToolCallData struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// hookPayload is the JSON Goose writes to a hook's stdin.
//
// Only SessionID is read. The payload also carries an "event" name, but the hook
// verb is already encoded in the Entire subcommand (`entire hooks goose
// turn-end`), so switching on the verb instead keeps the integration working if
// Goose renames the field.
type hookPayload struct {
	SessionID  string `json:"session_id"`
	WorkingDir string `json:"working_dir,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
}

// hookMatcherEntry groups a set of commands behind an optional regex matcher.
// Matcher is omitted for the four lifecycle events Entire registers — none of
// them support matchers in a way that is useful to us, and an absent matcher
// means "run for every value".
type hookMatcherEntry struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

// hookCommand is a single command Goose executes. Only "command" is a supported
// type in v1.46.0.
type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}
