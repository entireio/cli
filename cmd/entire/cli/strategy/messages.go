package strategy

import (
	"fmt"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// MaxDescriptionLength is the maximum length for descriptions in commit messages
// before truncation occurs.
const MaxDescriptionLength = 60

// TruncateDescription truncates a string to maxLen runes, adding "..." if truncated.
// Uses rune-based slicing to avoid splitting multi-byte UTF-8 characters.
// If maxLen is less than 3, truncates without ellipsis.
func TruncateDescription(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return stringutil.TruncateRunes(s, maxLen, "")
	}
	return stringutil.TruncateRunes(s, maxLen, "...")
}

// FormatSubagentEndMessage formats a commit message for when a subagent completes.
// Format: "Completed '<agent-type>' agent: <description> (<tool-use-id>)"
//
// Edge cases:
//   - Empty description: "Completed '<agent-type>' agent (<tool-use-id>)"
//   - Empty agentType: "Completed agent: <description> (<tool-use-id>)"
//   - Both empty: "Task: <tool-use-id>"
func FormatSubagentEndMessage(agentType, description, toolUseID string) string {
	return formatSubagentMessage("Completed", agentType, description, toolUseID)
}

// FormatSubagentRunningMessage is FormatSubagentEndMessage's in-flight
// counterpart, for pending-list rows whose task record is still live.
func FormatSubagentRunningMessage(agentType, description, toolUseID string) string {
	return formatSubagentMessage("Running", agentType, description, toolUseID)
}

// formatSubagentMessage is a shared helper for start/end messages.
func formatSubagentMessage(verb, agentType, description, toolUseID string) string {
	// Both empty - fall back to simple format
	if agentType == "" && description == "" {
		return "Task: " + toolUseID
	}

	// Truncate description if needed
	if description != "" {
		description = TruncateDescription(description, MaxDescriptionLength)
	}

	// Build message based on what fields are present
	if agentType != "" && description != "" {
		return fmt.Sprintf("%s '%s' agent: %s (%s)", verb, agentType, description, toolUseID)
	}
	if agentType != "" {
		return fmt.Sprintf("%s '%s' agent (%s)", verb, agentType, toolUseID)
	}
	// agentType is empty, description is present
	return fmt.Sprintf("%s agent: %s (%s)", verb, description, toolUseID)
}
