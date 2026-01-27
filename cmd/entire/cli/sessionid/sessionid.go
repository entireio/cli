// Package sessionid provides session ID formatting and transformation functions.
// This package has minimal dependencies to avoid import cycles.
package sessionid

import (
	"time"
)

// EntireSessionID generates the full Entire session ID from an agent session UUID.
// The format is: YYYY-MM-DD-<agent-session-uuid>
func EntireSessionID(agentSessionUUID string) string {
	return time.Now().Format("2006-01-02") + "-" + agentSessionUUID
}

// ModelSessionID extracts the agent session UUID from an Entire session ID.
// The Entire session ID format is: YYYY-MM-DD-<agent-session-uuid>
// Returns the original string if it doesn't match the expected format.
func ModelSessionID(entireSessionID string) string {
	// Expected format: YYYY-MM-DD-<agent-uuid> (11 chars prefix: "2026-01-23-")
	if len(entireSessionID) > 11 && entireSessionID[4] == '-' && entireSessionID[7] == '-' && entireSessionID[10] == '-' {
		return entireSessionID[11:]
	}
	// Return as-is if not in expected format (backwards compatibility)
	return entireSessionID
}
