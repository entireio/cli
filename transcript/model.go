package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
)

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
	Version            int
	Type               string // TypeUser, TypeAssistant, or a future additive type
	Agent              string
	CLIVersion         string
	Timestamp          string
	ID                 string
	Model              string
	InputTokens        int64 // excludes CacheReadTokens and CacheWriteTokens
	OutputTokens       int64
	CacheReadTokens    int64
	CacheWriteTokens   int64
	CacheWrite5mTokens *int64 // nil means the split is unknown
	CacheWrite1hTokens *int64 // nil means the split is unknown
	ReasoningTokens    int64
	Blocks             []Block
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
// Its JSON encoding is the stored "result" object.
type ToolResult struct {
	Output     string          `json:"output"`
	Status     string          `json:"status"` // ToolStatusSuccess or ToolStatusError
	File       *ToolResultFile `json:"file,omitempty"`
	MatchCount int             `json:"matchCount,omitempty"`
	// Agent identifies the subagent this tool call launched, when the agent
	// records one in the parent transcript.
	Agent *ToolResultAgent `json:"agent,omitempty"`
	// Images are the images the tool returned, each verbatim in the agent's
	// native shape, in source order.
	Images []json.RawMessage `json:"images,omitempty"`
}

// ToolResultAgent identifies a subagent launched by a tool call.
type ToolResultAgent struct {
	// ID is the agent's own ID for the child session or thread, never the
	// tool call ID (that is the tool_use block's id).
	ID string `json:"id"`
}

// UnmarshalJSON decodes a tool result. An "agent" that is not an object with
// a non-empty string "id" decodes as absent rather than failing the result,
// so a malformed one neither drops the block around it nor folds into an
// empty agent. Likewise, "images" that is not an array decodes as absent.
func (r *ToolResult) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil // as encoding/json leaves a struct on null
	}
	type plain ToolResult
	var w struct {
		plain

		Agent  json.RawMessage `json:"agent"`  // shadows plain.Agent
		Images json.RawMessage `json:"images"` // shadows plain.Images
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("decoding tool result: %w", err)
	}
	*r = ToolResult(w.plain)
	var images []json.RawMessage
	if json.Unmarshal(w.Images, &images) == nil && len(images) > 0 {
		r.Images = images
	}
	var agent struct {
		ID json.RawMessage `json:"id"`
	}
	var id string
	if json.Unmarshal(w.Agent, &agent) == nil && json.Unmarshal(agent.ID, &id) == nil && id != "" {
		r.Agent = &ToolResultAgent{ID: id}
	}
	return nil
}

// ToolResultFile carries file metadata from Read/Edit tool results.
type ToolResultFile struct {
	FilePath string `json:"filePath"`
	NumLines int    `json:"numLines,omitempty"`
}
