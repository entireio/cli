package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure ClaudeCodeAgent implements StatusLineSupport.
var _ agent.StatusLineSupport = (*ClaudeCodeAgent)(nil)

// statusLineSettingsKey is the top-level key Claude Code reads for its status
// line (https://code.claude.com/docs/en/statusline).
const statusLineSettingsKey = "statusLine"

// statusLineProductionCommand is the command Claude Code runs for the status
// line in normal (installed-binary) mode. Claude Code pipes the status-line
// JSON payload to it on stdin; `entire trail status` reads the workspace dir
// from that payload.
const statusLineProductionCommand = "entire trail status"

// claudeStatusLine is the shape Claude Code expects under the statusLine key.
type claudeStatusLine struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// statusLineCommand returns the status-line command for the given mode.
func statusLineCommand(localDev bool) string {
	if localDev {
		return localDevHookCmdPrefix + "trail status"
	}
	return statusLineProductionCommand
}

// isEntireStatusLineCommand reports whether a configured status-line command is
// one Entire installed (production or local-dev), so upgrades and removals only
// ever touch Entire's own entry — never a status line the user configured.
func isEntireStatusLineCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	for _, prefix := range entireHookPrefixes {
		if strings.HasPrefix(command, prefix) && strings.Contains(command, "trail status") {
			return true
		}
	}
	return false
}

// InstallStatusLine writes Entire's trail status line into .claude/settings.json.
// It installs only when no status line exists or the existing one is Entire's
// (upgrading the command if it changed); a status line the user configured
// themselves is left untouched and the call reports false.
func (c *ClaudeCodeAgent) InstallStatusLine(ctx context.Context, localDev bool) (bool, error) {
	settingsPath := claudeSettingsPath(ctx)

	rawSettings, err := readClaudeSettingsMap(settingsPath)
	if err != nil {
		return false, err
	}

	command := statusLineCommand(localDev)
	if existing, ok := parseExistingStatusLine(rawSettings); ok {
		if !isEntireStatusLineCommand(existing.Command) {
			// A status line the user owns — never clobber it.
			return false, nil
		}
		if existing.Command == command {
			return false, nil // already installed and current
		}
	}

	entry, err := jsonutil.MarshalWithNoHTMLEscape(claudeStatusLine{Type: "command", Command: command})
	if err != nil {
		return false, fmt.Errorf("failed to marshal statusLine: %w", err)
	}
	rawSettings[statusLineSettingsKey] = entry

	if err := writeClaudeSettingsMap(settingsPath, rawSettings); err != nil {
		return false, err
	}
	return true, nil
}

// UninstallStatusLine removes Entire's status line, leaving a user-configured
// status line in place.
func (c *ClaudeCodeAgent) UninstallStatusLine(ctx context.Context) error {
	settingsPath := claudeSettingsPath(ctx)

	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed name
	if err != nil {
		return nil //nolint:nilerr // no settings file means nothing to uninstall
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	existing, ok := parseExistingStatusLine(rawSettings)
	if !ok || !isEntireStatusLineCommand(existing.Command) {
		return nil // not ours (or absent) — leave it alone
	}

	delete(rawSettings, statusLineSettingsKey)
	return writeClaudeSettingsMap(settingsPath, rawSettings)
}

// IsStatusLineInstalled reports whether Entire's status line is configured.
func (c *ClaudeCodeAgent) IsStatusLineInstalled(ctx context.Context) bool {
	data, err := os.ReadFile(claudeSettingsPath(ctx))
	if err != nil {
		return false
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return false
	}
	existing, ok := parseExistingStatusLine(rawSettings)
	return ok && isEntireStatusLineCommand(existing.Command)
}

// parseExistingStatusLine extracts the configured status line, if any. A
// present-but-unparseable value reports ok=true with an empty command so
// callers treat it as a user-owned status line and leave it untouched.
func parseExistingStatusLine(rawSettings map[string]json.RawMessage) (claudeStatusLine, bool) {
	raw, ok := rawSettings[statusLineSettingsKey]
	if !ok {
		return claudeStatusLine{}, false
	}
	var existing claudeStatusLine
	if err := json.Unmarshal(raw, &existing); err != nil {
		return claudeStatusLine{}, true
	}
	return existing, true
}

// claudeSettingsPath resolves the project .claude/settings.json path from the
// repo root, falling back to the working directory outside a git repo (tests).
func claudeSettingsPath(ctx context.Context) string {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	return filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)
}

// readClaudeSettingsMap reads settings.json into a raw key map (preserving
// unknown keys), returning an empty map when the file does not exist.
func readClaudeSettingsMap(settingsPath string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed name
	if err != nil {
		return make(map[string]json.RawMessage), nil //nolint:nilerr // missing file → empty settings
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return nil, fmt.Errorf("failed to parse settings.json: %w", err)
	}
	if rawSettings == nil {
		rawSettings = make(map[string]json.RawMessage)
	}
	return rawSettings, nil
}

// writeClaudeSettingsMap writes a raw settings map back to disk with the
// project's indentation, creating the .claude directory as needed.
func writeClaudeSettingsMap(settingsPath string, rawSettings map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}
	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}
	return nil
}
