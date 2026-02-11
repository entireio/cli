package opencode

// Tool names used in OpenCode transcripts
const (
	ToolWrite      = "Write"
	ToolEdit       = "Edit"
	ToolApplyPatch = "ApplyPatch"
	ToolMultiEdit  = "MultiEdit"
)

// FileModificationTools lists tools that create or modify files
var FileModificationTools = []string{
	ToolWrite,
	ToolEdit,
	ToolApplyPatch,
	ToolMultiEdit,
}
