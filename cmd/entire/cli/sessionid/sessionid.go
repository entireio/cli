// Package sessionid provides session ID formatting and transformation functions.
// This package has minimal dependencies to avoid import cycles.
package sessionid

// EntireSessionID returns the Entire session ID from an agent session UUID.
// This is now an identity function - the agent session ID IS the Entire session ID.
// This simplification removes the non-deterministic date prefix that made it
// impossible to derive the Entire session ID from just the agent session ID.
func EntireSessionID(agentSessionUUID string) string {
	return agentSessionUUID
}

// ModelSessionID extracts the agent session UUID from an Entire session ID.
// Since Entire session ID = agent session ID (identity), this returns the input unchanged.
// For backwards compatibility with old date-prefixed session IDs (YYYY-MM-DD-<uuid>),
// it strips the date prefix if present.
func ModelSessionID(entireSessionID string) string {
	// Check for legacy format: YYYY-MM-DD-<agent-uuid> (11 chars prefix: "2026-01-23-")
	if len(entireSessionID) > 11 && entireSessionID[4] == '-' && entireSessionID[7] == '-' && entireSessionID[10] == '-' {
		return entireSessionID[11:]
	}
	// Return as-is (new format: agent session ID = entire session ID)
	return entireSessionID
}
