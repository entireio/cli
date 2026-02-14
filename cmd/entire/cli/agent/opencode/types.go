package opencode

// Tool names used in OpenCode transcripts
const (
	ToolWrite      = "Write"
	ToolEdit       = "Edit"
	ToolApplyPatch = "ApplyPatch"
	ToolMultiEdit  = "MultiEdit"
)

// FileModificationTools returns tools that create or modify files.
// Returns a new slice each call to prevent mutation.
func FileModificationTools() []string {
	return []string{
		ToolWrite,
		ToolEdit,
		ToolApplyPatch,
		ToolMultiEdit,
	}
}

// pluginPayload is the JSON structure sent from the Entire plugin to hook handlers.
// The OpenCode plugin pipes this via BunShell stdin.
type pluginPayload struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
}
