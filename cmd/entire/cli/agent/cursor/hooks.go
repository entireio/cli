package cursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// CursorHooksFileName is the hooks config filename under .cursor/
const CursorHooksFileName = "hooks.json"

// Ensure CursorAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*CursorAgent)(nil)
	_ agent.HookHandler = (*CursorAgent)(nil)
)

// Hook names - subcommands under `entire hooks cursor`
const (
	HookNameSessionStart       = "session-start"
	HookNameSessionEnd         = "session-end"
	HookNameBeforeSubmitPrompt = "before-submit-prompt"
	HookNameStop               = "stop"
	HookNamePreTask            = "pre-task"
	HookNamePostTask           = "post-task"
	HookNamePostTodo           = "post-todo"
)

// entireHookPrefixes identify Entire hook commands in .cursor/hooks.json
var entireHookPrefixes = []string{
	"entire ",
	"go run ${CURSOR_PROJECT_DIR}/cmd/entire/main.go ",
}

// GetHookNames returns the hook verbs Cursor supports.
func (c *CursorAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameBeforeSubmitPrompt,
		HookNameStop,
		HookNamePreTask,
		HookNamePostTask,
		HookNamePostTodo,
	}
}

// InstallHooks installs Cursor hooks in .cursor/hooks.json only. Does not touch .claude/*.
func (c *CursorAgent) InstallHooks(localDev bool, force bool) (int, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Fallback when not in git repo (e.g. tests)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".cursor", CursorHooksFileName)

	var file CursorHooksFile
	file.Version = 1
	file.Hooks = CursorHooks{}

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path from repo root + constant
	if readErr == nil {
		if err := json.Unmarshal(existingData, &file); err != nil {
			return 0, fmt.Errorf("failed to parse existing .cursor/hooks.json: %w", err)
		}
	}

	if file.Hooks.SessionStart == nil {
		file.Hooks.SessionStart = []CursorHookEntry{}
	}
	if file.Hooks.SessionEnd == nil {
		file.Hooks.SessionEnd = []CursorHookEntry{}
	}
	if file.Hooks.BeforeSubmitPrompt == nil {
		file.Hooks.BeforeSubmitPrompt = []CursorHookEntry{}
	}
	if file.Hooks.Stop == nil {
		file.Hooks.Stop = []CursorHookEntry{}
	}
	if file.Hooks.PreToolUse == nil {
		file.Hooks.PreToolUse = []CursorHookEntry{}
	}
	if file.Hooks.PostToolUse == nil {
		file.Hooks.PostToolUse = []CursorHookEntry{}
	}

	if force {
		file.Hooks.SessionStart = removeEntireHooksCursor(file.Hooks.SessionStart)
		file.Hooks.SessionEnd = removeEntireHooksCursor(file.Hooks.SessionEnd)
		file.Hooks.BeforeSubmitPrompt = removeEntireHooksCursor(file.Hooks.BeforeSubmitPrompt)
		file.Hooks.Stop = removeEntireHooksCursor(file.Hooks.Stop)
		file.Hooks.PreToolUse = removeEntireHooksCursor(file.Hooks.PreToolUse)
		file.Hooks.PostToolUse = removeEntireHooksCursor(file.Hooks.PostToolUse)
	}

	cmdPrefix := "entire hooks cursor "
	if localDev {
		cmdPrefix = "go run ${CURSOR_PROJECT_DIR}/cmd/entire/main.go hooks cursor "
	}

	count := 0
	if !cursorHookExists(file.Hooks.SessionStart, cmdPrefix+HookNameSessionStart) {
		file.Hooks.SessionStart = append(file.Hooks.SessionStart, CursorHookEntry{Command: cmdPrefix + HookNameSessionStart})
		count++
	}
	if !cursorHookExists(file.Hooks.SessionEnd, cmdPrefix+HookNameSessionEnd) {
		file.Hooks.SessionEnd = append(file.Hooks.SessionEnd, CursorHookEntry{Command: cmdPrefix + HookNameSessionEnd})
		count++
	}
	if !cursorHookExists(file.Hooks.BeforeSubmitPrompt, cmdPrefix+HookNameBeforeSubmitPrompt) {
		file.Hooks.BeforeSubmitPrompt = append(file.Hooks.BeforeSubmitPrompt, CursorHookEntry{Command: cmdPrefix + HookNameBeforeSubmitPrompt})
		count++
	}
	if !cursorHookExists(file.Hooks.Stop, cmdPrefix+HookNameStop) {
		file.Hooks.Stop = append(file.Hooks.Stop, CursorHookEntry{Command: cmdPrefix + HookNameStop})
		count++
	}
	if !cursorHookExists(file.Hooks.PreToolUse, cmdPrefix+HookNamePreTask) {
		file.Hooks.PreToolUse = append(file.Hooks.PreToolUse, CursorHookEntry{Command: cmdPrefix + HookNamePreTask})
		count++
	}
	if !cursorHookExists(file.Hooks.PostToolUse, cmdPrefix+HookNamePostTask) {
		file.Hooks.PostToolUse = append(file.Hooks.PostToolUse, CursorHookEntry{Command: cmdPrefix + HookNamePostTask})
		count++
	}
	if !cursorHookExists(file.Hooks.PostToolUse, cmdPrefix+HookNamePostTodo) {
		file.Hooks.PostToolUse = append(file.Hooks.PostToolUse, CursorHookEntry{Command: cmdPrefix + HookNamePostTodo})
		count++
	}

	if count == 0 {
		return 0, nil
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .cursor directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(file, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write .cursor/hooks.json: %w", err)
	}
	return count, nil
}

// UninstallHooks removes Entire hooks from .cursor/hooks.json.
func (c *CursorAgent) UninstallHooks() error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	settingsPath := filepath.Join(repoRoot, ".cursor", CursorHooksFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path from repo root + constant
	if err != nil {
		return nil //nolint:nilerr // No file means nothing to uninstall
	}

	var file CursorHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("failed to parse .cursor/hooks.json: %w", err)
	}

	file.Hooks.SessionStart = removeEntireHooksCursor(file.Hooks.SessionStart)
	file.Hooks.SessionEnd = removeEntireHooksCursor(file.Hooks.SessionEnd)
	file.Hooks.BeforeSubmitPrompt = removeEntireHooksCursor(file.Hooks.BeforeSubmitPrompt)
	file.Hooks.Stop = removeEntireHooksCursor(file.Hooks.Stop)
	file.Hooks.PreToolUse = removeEntireHooksCursor(file.Hooks.PreToolUse)
	file.Hooks.PostToolUse = removeEntireHooksCursor(file.Hooks.PostToolUse)

	output, err := jsonutil.MarshalIndentWithNewline(file, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks: %w", err)
	}
	return os.WriteFile(settingsPath, output, 0o600)
}

// AreHooksInstalled checks if any Entire hook is present in .cursor/hooks.json.
func (c *CursorAgent) AreHooksInstalled() bool {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	settingsPath := filepath.Join(repoRoot, ".cursor", CursorHooksFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path from repo root + constant
	if err != nil {
		return false
	}
	var file CursorHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		return false
	}
	return cursorHookExists(file.Hooks.Stop, "entire hooks cursor "+HookNameStop) ||
		cursorHookExists(file.Hooks.Stop, "go run ${CURSOR_PROJECT_DIR}/cmd/entire/main.go hooks cursor "+HookNameStop)
}

// GetSupportedHooks returns the hook types Cursor supports.
func (c *CursorAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookSessionEnd,
		agent.HookUserPromptSubmit,
		agent.HookStop,
		agent.HookPreToolUse,
		agent.HookPostToolUse,
	}
}

func cursorHookExists(entries []CursorHookEntry, command string) bool {
	for _, e := range entries {
		if e.Command == command {
			return true
		}
	}
	return false
}

func isEntireHookCursor(command string) bool {
	for _, prefix := range entireHookPrefixes {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	return false
}

func removeEntireHooksCursor(entries []CursorHookEntry) []CursorHookEntry {
	out := make([]CursorHookEntry, 0, len(entries))
	for _, e := range entries {
		if !isEntireHookCursor(e.Command) {
			out = append(out, e)
		}
	}
	return out
}
