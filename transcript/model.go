package transcript

import "encoding/json"

// Block types. Other values pass through verbatim; see Block.Raw.
const (
	BlockText    = ContentTypeText
	BlockToolUse = ContentTypeToolUse
	BlockImage   = "image"
)

// Tool result statuses.
const (
	ToolStatusSuccess = "success"
	ToolStatusError   = "error"
)

// Line is one decoded line of the Entire Transcript Format.
type Line struct {
	Version      int
	Type         string // TypeUser, TypeAssistant, or a future additive type
	Agent        string
	CLIVersion   string
	Timestamp    string
	ID           string
	InputTokens  int
	OutputTokens int
	Blocks       []Block
}

// Block is one content block of a Line.
type Block struct {
	Type   string // BlockText, BlockToolUse, BlockImage, or a passthrough type
	ID     string
	Text   string
	Name   string
	Input  json.RawMessage
	Result *ToolResult
	Raw    json.RawMessage // the block exactly as stored
}

// ToolResult is the result inlined into the tool_use block that requested it.
type ToolResult struct {
	Output     string
	Status     string // ToolStatusSuccess or ToolStatusError
	File       *ToolResultFile
	MatchCount int
}

// ToolResultFile carries file metadata from Read/Edit tool results.
type ToolResultFile struct {
	FilePath string
	NumLines int
}
