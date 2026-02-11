package pi

import "encoding/json"

// piHookInput represents the JSON input from pi extension hooks.
type piHookInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Prompt         string          `json:"prompt,omitempty"`
	ModifiedFiles  []string        `json:"modified_files,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	LeafID         string          `json:"leaf_id,omitempty"`
}
