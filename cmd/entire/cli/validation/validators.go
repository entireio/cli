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

// windowsReservedDeviceNames are the DOS device names Windows refuses as file
// names in any case, with any extension, and with trailing dots or spaces:
// "CON", "con.txt" and "NUL." all name the console or null device. An ID that
// hits one cannot be materialized on a Windows checkout, and os.Root refuses
// it outright where a plain filepath.Join would quietly open the device. IDs
// travel inside checkpoint metadata and are restored on whatever machine pulls
// them, so the check runs on every platform, not just Windows.
var windowsReservedDeviceNames = func() map[string]struct{} {
	names := map[string]struct{}{"CON": {}, "PRN": {}, "AUX": {}, "NUL": {}, "CONIN$": {}, "CONOUT$": {}}
	for _, d := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
		names["COM"+d] = struct{}{}
		names["LPT"+d] = struct{}{}
	}
	return names
}()

// isWindowsReservedDeviceName applies Windows' own rule: the name up to the
// first dot, with trailing spaces and dots stripped, compared case-insensitively.
func isWindowsReservedDeviceName(id string) bool {
	base := id
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimRight(base, " .")
	_, reserved := windowsReservedDeviceNames[strings.ToUpper(base)]
	return reserved
}

// ValidateSessionID validates that a session ID doesn't contain path separators
// or other unsafe characters for use in file paths.
// This prevents path traversal attacks when session IDs are used in file paths.
func ValidateSessionID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("session ID cannot be empty")
	}
	if strings.HasPrefix(id, "-") {
		return fmt.Errorf("invalid session ID %q: starts with dash", id)
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
	if isWindowsReservedDeviceName(id) {
		return fmt.Errorf("invalid session ID %q: Windows reserved device name", id)
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
	if isWindowsReservedDeviceName(id) {
		return fmt.Errorf("invalid tool use ID %q: Windows reserved device name", id)
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
	if isWindowsReservedDeviceName(id) {
		return fmt.Errorf("invalid agent ID %q: Windows reserved device name", id)
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
	if !pathSafeRegex.MatchString(id) {
		return fmt.Errorf("invalid agent session ID %q: must be alphanumeric with underscores/hyphens only", id)
	}
	if isWindowsReservedDeviceName(id) {
		return fmt.Errorf("invalid agent session ID %q: Windows reserved device name", id)
	}
	return nil
}
