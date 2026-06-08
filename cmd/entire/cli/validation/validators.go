// Package validation provides input validation functions for the Entire CLI.
// This package has no dependencies to avoid import cycles.
package validation

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// pathSafeRegex matches alphanumeric characters, underscores, and hyphens only.
// Used to validate IDs that will be used in file paths.
var pathSafeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateSessionID validates that a session ID doesn't contain path separators
// or other unsafe characters for use in file paths.
// This prevents path traversal attacks when session IDs are used in file paths.
func ValidateSessionID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("session ID cannot be empty")
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("invalid session ID %q: contains path separators", id)
	}
	// A bare "." or ".." is separator-free but still traverses when used as a
	// path segment (e.g. an agent that uses the ID as a directory component).
	if id == "." || id == ".." {
		return fmt.Errorf("invalid session ID %q: reserved path segment", id)
	}
	// Reject the Windows volume separator. A drive-relative path like "C:foo" is
	// separator-free and filepath.IsAbs reports it as non-absolute, yet
	// filepath.Join discards the base directory when the appended element
	// carries a volume name — escaping the intended directory on Windows.
	if strings.Contains(id, ":") {
		return fmt.Errorf("invalid session ID %q: contains volume separator", id)
	}
	// Reject glob metacharacters. Session IDs are interpolated into
	// filepath.Glob patterns in several places (agent transcript lookup,
	// session-state cleanup); "*"/"?"/"[" could match and act on unrelated files.
	if strings.ContainsAny(id, "*?[") {
		return fmt.Errorf("invalid session ID %q: contains glob metacharacters", id)
	}
	// Defense in depth against platform-specific absolute forms (e.g. Windows
	// drive paths) that the separator check above may not catch.
	if filepath.IsAbs(id) || filepath.VolumeName(id) != "" {
		return fmt.Errorf("invalid session ID %q: must not be an absolute path", id)
	}
	return nil
}

// RejectAbsoluteSessionID returns an error if id is an absolute path (including
// Windows drive-relative forms). Agent ResolveSessionFile implementations call
// this as a defense-in-depth guard: an absolute agentSessionID is
// attacker-influenceable (it can originate from checkpoint metadata on the
// shared entire/checkpoints/v1 branch) and, if joined or returned verbatim,
// could resolve to a path outside the session directory — letting the
// resume/rewind write path overwrite arbitrary files.
//
// This is intentionally narrower than ValidateSessionID: it only rejects
// absolute paths, not "../" traversal or separators. Restore-path callers must
// still call ValidateSessionID for the full guard; this closes the absolute
// footgun at the function itself so it cannot regress out from under a future
// caller that forgets.
func RejectAbsoluteSessionID(id string) error {
	if filepath.IsAbs(id) || filepath.VolumeName(id) != "" {
		return fmt.Errorf("session ID must be relative, got absolute path: %q", id)
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
