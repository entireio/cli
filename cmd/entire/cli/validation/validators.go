// Package validation provides input validation functions for the Entire CLI.
// This package has no dependencies to avoid import cycles.
package validation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// pathSafeRegex matches alphanumeric characters, underscores, and hyphens only.
// Used to validate IDs that will be used in file paths.
var pathSafeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateSessionID validates that a session ID doesn't contain path separators.
// This prevents path traversal attacks when session IDs are used in file paths.
func ValidateSessionID(id string) error {
	if id == "" {
		return errors.New("session ID cannot be empty")
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("invalid session ID %q: contains path separators", id)
	}
	return nil
}

// ValidateToolUseID validates that a tool use ID contains only safe characters for paths.
// Tool use IDs can be UUIDs or prefixed identifiers like "toolu_xxx".
func ValidateToolUseID(id string) error {
	if id == "" {
		return nil // Empty is allowed (optional field)
	}
	if !pathSafeRegex.MatchString(id) {
		return fmt.Errorf("invalid tool use ID %q: must be alphanumeric with underscores/hyphens only", id)
	}
	return nil
}

// ValidateAgentID validates that an agent ID contains only safe characters for paths.
func ValidateAgentID(id string) error {
	if id == "" {
		return nil // Empty is allowed (optional field)
	}
	if !pathSafeRegex.MatchString(id) {
		return fmt.Errorf("invalid agent ID %q: must be alphanumeric with underscores/hyphens only", id)
	}
	return nil
}

// ValidateAgentSessionID validates that an agent session ID contains only safe characters for paths.
// Agent session IDs can be UUIDs (Claude Code), test identifiers, or other formats depending on the agent.
// This prevents path traversal attacks when the ID is used in file path construction.
func ValidateAgentSessionID(id string) error {
	if id == "" {
		return errors.New("agent session ID cannot be empty")
	}
	if !pathSafeRegex.MatchString(id) {
		return fmt.Errorf("invalid agent session ID %q: must be alphanumeric with underscores/hyphens only", id)
	}
	return nil
}
