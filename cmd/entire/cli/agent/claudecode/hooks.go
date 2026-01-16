package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/paths"
)

// Ensure ClaudeCodeAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*ClaudeCodeAgent)(nil)
	_ agent.HookHandler = (*ClaudeCodeAgent)(nil)
)

// Claude Code hook names - these become subcommands under `entire hooks claude-code`
const (
	HookNameSessionStart     = "session-start"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNamePreTask          = "pre-task"
	HookNamePostTask         = "post-task"
	HookNamePostTodo         = "post-todo"
)

// ClaudeSettingsFileName is the settings file used by Claude Code.
// This is Claude-specific and not shared with other agents.
const ClaudeSettingsFileName = "settings.json"

// metadataDenyRule blocks Claude from reading Entire session metadata
const metadataDenyRule = "Read(./.entire/metadata/**)"

// GetHookNames returns the hook verbs Claude Code supports.
// These become subcommands: entire hooks claude-code <verb>
func (c *ClaudeCodeAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNamePreTask,
		HookNamePostTask,
		HookNamePostTodo,
	}
}

// entireHookPatterns are regex patterns that identify Entire hooks
var entireHookPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^entire `),
	regexp.MustCompile(`^go run \$\{CLAUDE_PROJECT_DIR\}/cmd/entire/main\.go `),
}

// InstallHooks installs Claude Code hooks in .claude/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (c *ClaudeCodeAgent) InstallHooks(localDev bool, force bool) (int, error) {
	// Use repo root instead of CWD to find .claude directory
	// This ensures hooks are installed correctly when run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when RepoRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, c.GetHookConfigPath())

	// Read existing settings if they exist
	var settings ClaudeSettings
	var rawSettings map[string]json.RawMessage

	// rawPermissions preserves unknown permission fields (e.g., "ask")
	var rawPermissions map[string]json.RawMessage

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from cwd + fixed path
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawSettings); err != nil {
			return 0, fmt.Errorf("failed to parse existing settings.json: %w", err)
		}
		if hooksRaw, ok := rawSettings["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &settings.Hooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in settings.json: %w", err)
			}
		}
		if permRaw, ok := rawSettings["permissions"]; ok {
			if err := json.Unmarshal(permRaw, &rawPermissions); err != nil {
				return 0, fmt.Errorf("failed to parse permissions in settings.json: %w", err)
			}
		}
	} else {
		rawSettings = make(map[string]json.RawMessage)
	}

	if rawPermissions == nil {
		rawPermissions = make(map[string]json.RawMessage)
	}

	// If force is true, remove all existing Entire hooks first
	if force {
		settings.Hooks.SessionStart = removeEntireHooks(settings.Hooks.SessionStart)
		settings.Hooks.Stop = removeEntireHooks(settings.Hooks.Stop)
		settings.Hooks.UserPromptSubmit = removeEntireHooks(settings.Hooks.UserPromptSubmit)
		settings.Hooks.PreToolUse = removeEntireHooksFromMatchers(settings.Hooks.PreToolUse)
		settings.Hooks.PostToolUse = removeEntireHooksFromMatchers(settings.Hooks.PostToolUse)
	}

	// Define hook commands
	var sessionStartCmd, stopCmd, userPromptSubmitCmd, preTaskCmd, postTaskCmd, postTodoCmd string
	if localDev {
		sessionStartCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code session-start"
		stopCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code stop"
		userPromptSubmitCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code user-prompt-submit"
		preTaskCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code pre-task"
		postTaskCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code post-task"
		postTodoCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code post-todo"
	} else {
		sessionStartCmd = "entire hooks claude-code session-start"
		stopCmd = "entire hooks claude-code stop"
		userPromptSubmitCmd = "entire hooks claude-code user-prompt-submit"
		preTaskCmd = "entire hooks claude-code pre-task"
		postTaskCmd = "entire hooks claude-code post-task"
		postTodoCmd = "entire hooks claude-code post-todo"
	}

	count := 0

	// Add hooks if they don't exist
	if !hookCommandExists(settings.Hooks.SessionStart, sessionStartCmd) {
		settings.Hooks.SessionStart = addHookToMatcher(settings.Hooks.SessionStart, "", sessionStartCmd)
		count++
	}
	if !hookCommandExists(settings.Hooks.Stop, stopCmd) {
		settings.Hooks.Stop = addHookToMatcher(settings.Hooks.Stop, "", stopCmd)
		count++
	}
	if !hookCommandExists(settings.Hooks.UserPromptSubmit, userPromptSubmitCmd) {
		settings.Hooks.UserPromptSubmit = addHookToMatcher(settings.Hooks.UserPromptSubmit, "", userPromptSubmitCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(settings.Hooks.PreToolUse, "Task", preTaskCmd) {
		settings.Hooks.PreToolUse = addHookToMatcher(settings.Hooks.PreToolUse, "Task", preTaskCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(settings.Hooks.PostToolUse, "Task", postTaskCmd) {
		settings.Hooks.PostToolUse = addHookToMatcher(settings.Hooks.PostToolUse, "Task", postTaskCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(settings.Hooks.PostToolUse, "TodoWrite", postTodoCmd) {
		settings.Hooks.PostToolUse = addHookToMatcher(settings.Hooks.PostToolUse, "TodoWrite", postTodoCmd)
		count++
	}

	// Add permissions.deny rule if not present
	permissionsChanged := false
	var denyRules []string
	if denyRaw, ok := rawPermissions["deny"]; ok {
		if err := json.Unmarshal(denyRaw, &denyRules); err != nil {
			return 0, fmt.Errorf("failed to parse permissions.deny in settings.json: %w", err)
		}
	}
	if !slices.Contains(denyRules, metadataDenyRule) {
		denyRules = append(denyRules, metadataDenyRule)
		denyJSON, err := json.Marshal(denyRules)
		if err != nil {
			return 0, fmt.Errorf("failed to marshal permissions.deny: %w", err)
		}
		rawPermissions["deny"] = denyJSON
		permissionsChanged = true
	}

	if count == 0 && !permissionsChanged {
		return 0, nil // All hooks and permissions already installed
	}

	// Marshal hooks and update raw settings
	hooksJSON, err := json.Marshal(settings.Hooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	// Marshal permissions and update raw settings
	permJSON, err := json.Marshal(rawPermissions)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal permissions: %w", err)
	}
	rawSettings["permissions"] = permJSON

	// Write back to file
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .claude directory: %w", err)
	}

	output, err := json.MarshalIndent(rawSettings, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write settings.json: %w", err)
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from Claude Code settings.
func (c *ClaudeCodeAgent) UninstallHooks() error {
	// Implementation would remove our hooks from .claude/settings.json
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (c *ClaudeCodeAgent) AreHooksInstalled() bool {
	// Use repo root to find .claude directory when run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, c.GetHookConfigPath())
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return false
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}

	// Check for at least one of our hooks (new or old format)
	return hookCommandExists(settings.Hooks.Stop, "entire hooks claude-code stop") ||
		hookCommandExists(settings.Hooks.Stop, "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code stop") ||
		// Backwards compatibility: check for old hook formats
		hookCommandExists(settings.Hooks.Stop, "entire hooks claudecode stop") ||
		hookCommandExists(settings.Hooks.Stop, "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claudecode stop") ||
		hookCommandExists(settings.Hooks.Stop, "entire rewind claude-hook --stop") ||
		hookCommandExists(settings.Hooks.Stop, "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go rewind claude-hook --stop")
}

// GetSupportedHooks returns the hook types Claude Code supports.
func (c *ClaudeCodeAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookUserPromptSubmit,
		agent.HookStop,
		agent.HookPreToolUse,
		agent.HookPostToolUse,
	}
}

// Helper functions for hook management

func hookCommandExists(matchers []ClaudeHookMatcher, command string) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func hookCommandExistsWithMatcher(matchers []ClaudeHookMatcher, matcherName, command string) bool {
	for _, matcher := range matchers {
		if matcher.Matcher == matcherName {
			for _, hook := range matcher.Hooks {
				if hook.Command == command {
					return true
				}
			}
		}
	}
	return false
}

func addHookToMatcher(matchers []ClaudeHookMatcher, matcherName, command string) []ClaudeHookMatcher {
	entry := ClaudeHookEntry{
		Type:    "command",
		Command: command,
	}

	// If no matcher name, add to a matcher with empty string
	if matcherName == "" {
		for i, matcher := range matchers {
			if matcher.Matcher == "" {
				matchers[i].Hooks = append(matchers[i].Hooks, entry)
				return matchers
			}
		}
		return append(matchers, ClaudeHookMatcher{
			Matcher: "",
			Hooks:   []ClaudeHookEntry{entry},
		})
	}

	// Find or create matcher with the given name
	for i, matcher := range matchers {
		if matcher.Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}

	return append(matchers, ClaudeHookMatcher{
		Matcher: matcherName,
		Hooks:   []ClaudeHookEntry{entry},
	})
}

// isEntireHook checks if a command is an Entire hook (old or new format)
func isEntireHook(command string) bool {
	for _, pattern := range entireHookPatterns {
		if pattern.MatchString(command) {
			return true
		}
	}
	return false
}

// removeEntireHooks removes all Entire hooks from a list of matchers (for simple hooks like Stop)
func removeEntireHooks(matchers []ClaudeHookMatcher) []ClaudeHookMatcher {
	result := make([]ClaudeHookMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		filteredHooks := make([]ClaudeHookEntry, 0, len(matcher.Hooks))
		for _, hook := range matcher.Hooks {
			if !isEntireHook(hook.Command) {
				filteredHooks = append(filteredHooks, hook)
			}
		}
		// Only keep the matcher if it has hooks remaining
		if len(filteredHooks) > 0 {
			matcher.Hooks = filteredHooks
			result = append(result, matcher)
		}
	}
	return result
}

// removeEntireHooksFromMatchers removes Entire hooks from tool-use matchers (PreToolUse, PostToolUse)
// This handles the nested structure where hooks are grouped by tool matcher (e.g., "Task", "TodoWrite")
func removeEntireHooksFromMatchers(matchers []ClaudeHookMatcher) []ClaudeHookMatcher {
	// Same logic as removeEntireHooks - both work on the same structure
	return removeEntireHooks(matchers)
}
