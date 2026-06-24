package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// ErrStatuslineConflict is returned by InstallStatusline when a non-Entire
// statusLine is already configured and force was not requested. Callers can
// detect it with errors.Is to warn-and-skip rather than fail.
var ErrStatuslineConflict = errors.New("a non-Entire statusLine is already configured")

// statuslineCommandMarker is the substring that identifies an Entire-owned
// statusLine command. It survives both the production (`entire …`) and
// local-dev (`${CLAUDE_PROJECT_DIR}/scripts/entire-dev …`) command forms, so
// AreHooksInstalled-style ownership checks can recognise our entry regardless
// of how it was installed.
const statuslineCommandMarker = "hooks claude-code statusline"

// claudeStatusLine mirrors the shape Claude Code expects under the top-level
// "statusLine" key in settings.json.
type claudeStatusLine struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Padding int    `json:"padding"`
}

// statuslineCommand returns the command Claude Code should run to render the
// Entire trail status line. In local-dev mode it routes through
// scripts/entire-dev (same prefix as the hooks) so an uncommitted CLI is used.
func statuslineCommand(localDev bool) string {
	if localDev {
		return localDevHookCmdPrefix + "hooks claude-code statusline"
	}
	return "entire hooks claude-code statusline"
}

// claudeSettingsPath resolves <repo-root>/.claude/settings.json, falling back
// to a CWD-relative path when not in a git repository (e.g. during tests).
func claudeSettingsPath(ctx context.Context) string {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		//nolint:forbidigo // explicit fallback when WorktreeRoot fails (tests run outside git repos)
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			repoRoot = cwd
		} else {
			repoRoot = "."
		}
	}
	return filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)
}

// InstallStatusline sets the Entire trail status line in .claude/settings.json.
//
// It never clobbers a user's own statusLine: if a foreign statusLine is already
// configured it returns an error (unless force is true) so the caller can warn
// and skip. If an Entire-owned statusLine is already present it is rewritten
// only when it differs (e.g. switching between production and local-dev).
//
// Returns true when settings.json was written, false when it was already
// up-to-date.
func (c *ClaudeCodeAgent) InstallStatusline(ctx context.Context, localDev bool, force bool) (bool, error) {
	settingsPath := claudeSettingsPath(ctx)

	rawSettings, err := readClaudeSettingsMap(settingsPath)
	if err != nil {
		return false, err
	}

	desired := claudeStatusLine{Type: "command", Command: statuslineCommand(localDev), Padding: 0}

	if existingRaw, ok := rawSettings["statusLine"]; ok {
		var existing claudeStatusLine
		// A statusLine may legitimately be a bare string in some configs; if it
		// doesn't parse as our object shape, treat it as foreign.
		if err := json.Unmarshal(existingRaw, &existing); err == nil && strings.Contains(existing.Command, statuslineCommandMarker) {
			if existing == desired {
				return false, nil // already up-to-date
			}
			// Entire-owned but stale (prod↔local-dev) — rewrite below.
		} else if !force {
			return false, fmt.Errorf("%w in %s; pass --force to replace", ErrStatuslineConflict, settingsPath)
		}
	}

	desiredJSON, err := jsonutil.MarshalWithNoHTMLEscape(desired)
	if err != nil {
		return false, fmt.Errorf("failed to marshal statusLine: %w", err)
	}
	rawSettings["statusLine"] = desiredJSON

	if err := writeClaudeSettingsMap(settingsPath, rawSettings); err != nil {
		return false, err
	}
	return true, nil
}

// UninstallStatusline removes the Entire trail status line from
// .claude/settings.json. A foreign statusLine is left untouched.
func (c *ClaudeCodeAgent) UninstallStatusline(ctx context.Context) error {
	settingsPath := claudeSettingsPath(ctx)

	data, err := os.ReadFile(settingsPath) //nolint:gosec // path constructed from repo root + fixed file name
	if err != nil {
		return nil //nolint:nilerr // no settings file means nothing to uninstall
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	existingRaw, ok := rawSettings["statusLine"]
	if !ok {
		return nil
	}
	var existing claudeStatusLine
	//nolint:nilerr // unparseable/foreign statusLine → intentionally leave it untouched
	if err := json.Unmarshal(existingRaw, &existing); err != nil || !strings.Contains(existing.Command, statuslineCommandMarker) {
		return nil // foreign statusLine — leave it alone
	}

	delete(rawSettings, "statusLine")
	return writeClaudeSettingsMap(settingsPath, rawSettings)
}

// IsStatuslineInstalled reports whether an Entire-owned statusLine is present.
func (c *ClaudeCodeAgent) IsStatuslineInstalled(ctx context.Context) bool {
	data, err := os.ReadFile(claudeSettingsPath(ctx))
	if err != nil {
		return false
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return false
	}
	existingRaw, ok := rawSettings["statusLine"]
	if !ok {
		return false
	}
	var existing claudeStatusLine
	if err := json.Unmarshal(existingRaw, &existing); err != nil {
		return false
	}
	return strings.Contains(existing.Command, statuslineCommandMarker)
}

// readClaudeSettingsMap reads settings.json into a raw key map, preserving all
// unknown keys. A missing file yields an empty map.
func readClaudeSettingsMap(settingsPath string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path constructed from repo root + fixed file name
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

// writeClaudeSettingsMap writes the raw settings map back to disk with the same
// formatting and permissions used by hook installation.
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
