package transcript

// Message type constants for transcript lines.
const (
	TypeUser      = "user"
	TypeAssistant = "assistant"
)

// Content type constants for content blocks within messages.
const (
	ContentTypeText    = "text"
	ContentTypeToolUse = "tool_use"
)

// unknownModel is the model reported when a source has usage but no model.
const unknownModel = "unknown"

// agentClaudeCode is the Options.Agent value for Claude Code transcripts.
const agentClaudeCode = "claude-code"
