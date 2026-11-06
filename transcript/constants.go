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

// syntheticModel is the placeholder Claude Code writes as message.model on
// entries it synthesizes, such as API errors. It names no model, so lines
// omit it.
const syntheticModel = "<synthetic>"

// agentClaudeCode is the Options.Agent value for Claude Code transcripts.
const agentClaudeCode = "claude-code"
