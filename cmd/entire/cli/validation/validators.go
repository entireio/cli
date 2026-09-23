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

var windowsReservedDeviceNameRegex = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|conin\$|conout\$|com[1-9¹²³]|lpt[1-9¹²³]) *(?:\..*)?$`)

// ValidateFileNameComponent rejects names whose meaning can change when used as
// one filesystem component on a supported platform. It deliberately does not
// apply identifier-only rules such as rejecting leading dashes or glob syntax.
//
// ONE component: a separator is rejected rather than split on. Callers do the
// splitting, and a validator that silently accepted "sub/../../../etc" because
// its only caller happened to split first is a trap for the second caller —
// note that "../x" passes every other rule here, and "../.." is caught only by
// the trailing-period check, so the mistake survives a casual smoke test.
func ValidateFileNameComponent(name string) error {
	if name == "" {
		return errors.New("file name component cannot be empty")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid file name component %q: contains path separators", name)
	}
	if reason := unsafeFileNameComponentReason(name); reason != "" {
		return fmt.Errorf("invalid file name component %q: %s", name, reason)
	}
	return nil
}

func unsafeFileNameComponentReason(name string) string {
	switch {
	// First, matching the order this helper replaced in ValidateFileNameComponent.
	// A drive-relative path like "C:foo" is separator-free and filepath.IsAbs
	// reports it as non-absolute, yet filepath.Join discards the base directory
	// when the appended element carries a volume — so on Windows it escapes.
	case strings.Contains(name, ":"):
		return "contains volume separator"
	case strings.ContainsFunc(name, func(r rune) bool { return r <= '\x1f' || r == '\x7f' }):
		return "contains control character"
	case name == "." || name == "..":
		return "reserved path segment"
	case strings.HasSuffix(name, " "):
		return "ends with space"
	case strings.HasSuffix(name, "."):
		return "ends with period"
	case windowsReservedDeviceNameRegex.MatchString(name):
		return "reserved Windows device name"
	default:
		return ""
	}
}

// ValidateSessionID validates that a session ID doesn't contain path separators
// or other unsafe characters for use in file paths.
// This prevents path traversal attacks when session IDs are used in file paths.
func ValidateSessionID(id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return errors.New("session ID cannot be empty")
	}
	if trimmed != id {
		return fmt.Errorf("invalid session ID %q: contains surrounding whitespace", id)
	}
	// Before the shared reason helper: "C:\\Windows\\System32" trips both the
	// separator rule and the volume-separator rule, and naming the separators is
	// the more useful of the two answers.
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("invalid session ID %q: contains path separators", id)
	}
	if reason := unsafeFileNameComponentReason(id); reason != "" {
		return fmt.Errorf("invalid session ID %q: %s", id, reason)
	}
	if strings.HasPrefix(id, "-") {
		return fmt.Errorf("invalid session ID %q: starts with dash", id)
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
	if strings.HasPrefix(id, "-") {
		return fmt.Errorf("invalid agent session ID %q: starts with dash", id)
	}
	if windowsReservedDeviceNameRegex.MatchString(id) {
		return fmt.Errorf("invalid agent session ID %q: reserved Windows device name", id)
	}
	if !pathSafeRegex.MatchString(id) {
		return fmt.Errorf("invalid agent session ID %q: must be alphanumeric with underscores/hyphens only", id)
	}
	return nil
}
