package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure ClaudeCodeAgent implements HookSupport and SkillInstaller
var _ agent.HookSupport = (*ClaudeCodeAgent)(nil)
var _ agent.SkillInstaller = (*ClaudeCodeAgent)(nil)

// Claude Code hook names - these become subcommands under `entire hooks claude-code`
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
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

// entireHookPrefixes are command prefixes that identify Entire hooks (both old and new formats)
var entireHookPrefixes = []string{
	"entire ",
	"go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go ",
}

// InstallHooks installs Claude Code hooks in .claude/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (c *ClaudeCodeAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	// Use repo root instead of CWD to find .claude directory
	// This ensures hooks are installed correctly when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)

	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types (e.g., "Notification", "SubagentStop")
	var rawHooks map[string]json.RawMessage

	// rawPermissions preserves unknown permission fields (e.g., "ask")
	var rawPermissions map[string]json.RawMessage

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + settings file name
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawSettings); err != nil {
			return 0, fmt.Errorf("failed to parse existing settings.json: %w", err)
		}
		if hooksRaw, ok := rawSettings["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
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

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}
	if rawPermissions == nil {
		rawPermissions = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PreToolUse", &preToolUse)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)

	// If force is true, remove all existing Entire hooks first
	if force {
		sessionStart = removeEntireHooks(sessionStart)
		sessionEnd = removeEntireHooks(sessionEnd)
		stop = removeEntireHooks(stop)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		preToolUse = removeEntireHooksFromMatchers(preToolUse)
		postToolUse = removeEntireHooksFromMatchers(postToolUse)
	}

	// Define hook commands
	var sessionStartCmd, sessionEndCmd, stopCmd, userPromptSubmitCmd, preTaskCmd, postTaskCmd, postTodoCmd string
	if localDev {
		sessionStartCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code session-start"
		sessionEndCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code session-end"
		stopCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code stop"
		userPromptSubmitCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code user-prompt-submit"
		preTaskCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code pre-task"
		postTaskCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code post-task"
		postTodoCmd = "go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks claude-code post-todo"
	} else {
		sessionStartCmd = "entire hooks claude-code session-start"
		sessionEndCmd = "entire hooks claude-code session-end"
		stopCmd = "entire hooks claude-code stop"
		userPromptSubmitCmd = "entire hooks claude-code user-prompt-submit"
		preTaskCmd = "entire hooks claude-code pre-task"
		postTaskCmd = "entire hooks claude-code post-task"
		postTodoCmd = "entire hooks claude-code post-todo"
	}

	count := 0

	// Add hooks if they don't exist
	if !hookCommandExists(sessionStart, sessionStartCmd) {
		sessionStart = addHookToMatcher(sessionStart, "", sessionStartCmd)
		count++
	}
	if !hookCommandExists(sessionEnd, sessionEndCmd) {
		sessionEnd = addHookToMatcher(sessionEnd, "", sessionEndCmd)
		count++
	}
	if !hookCommandExists(stop, stopCmd) {
		stop = addHookToMatcher(stop, "", stopCmd)
		count++
	}
	if !hookCommandExists(userPromptSubmit, userPromptSubmitCmd) {
		userPromptSubmit = addHookToMatcher(userPromptSubmit, "", userPromptSubmitCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(preToolUse, "Task", preTaskCmd) {
		preToolUse = addHookToMatcher(preToolUse, "Task", preTaskCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(postToolUse, "Task", postTaskCmd) {
		postToolUse = addHookToMatcher(postToolUse, "Task", postTaskCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(postToolUse, "TodoWrite", postTodoCmd) {
		postToolUse = addHookToMatcher(postToolUse, "TodoWrite", postTodoCmd)
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

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Marshal hooks and update raw settings
	hooksJSON, err := json.Marshal(rawHooks)
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

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write settings.json: %w", err)
	}

	return count, nil
}

// parseHookType parses a specific hook type from rawHooks into the target slice.
// Silently ignores parse errors (leaves target unchanged).
func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]ClaudeHookMatcher) {
	if data, ok := rawHooks[hookType]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []ClaudeHookMatcher) {
	if len(matchers) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := json.Marshal(matchers)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[hookType] = data
}

// UninstallHooks removes Entire hooks from Claude Code settings.
func (c *ClaudeCodeAgent) UninstallHooks(ctx context.Context) error {
	// Use repo root to find .claude directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No settings file means nothing to uninstall
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	// rawHooks preserves unknown hook types (e.g., "Notification", "SubagentStop")
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks: %w", err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PreToolUse", &preToolUse)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)

	// Remove Entire hooks from all hook types
	sessionStart = removeEntireHooks(sessionStart)
	sessionEnd = removeEntireHooks(sessionEnd)
	stop = removeEntireHooks(stop)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	preToolUse = removeEntireHooksFromMatchers(preToolUse)
	postToolUse = removeEntireHooksFromMatchers(postToolUse)

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Also remove the metadata deny rule from permissions
	var rawPermissions map[string]json.RawMessage
	if permRaw, ok := rawSettings["permissions"]; ok {
		if err := json.Unmarshal(permRaw, &rawPermissions); err != nil {
			// If parsing fails, just skip permissions cleanup
			rawPermissions = nil
		}
	}

	if rawPermissions != nil {
		if denyRaw, ok := rawPermissions["deny"]; ok {
			var denyRules []string
			if err := json.Unmarshal(denyRaw, &denyRules); err == nil {
				// Filter out the metadata deny rule
				filteredRules := make([]string, 0, len(denyRules))
				for _, rule := range denyRules {
					if rule != metadataDenyRule {
						filteredRules = append(filteredRules, rule)
					}
				}
				if len(filteredRules) > 0 {
					denyJSON, err := json.Marshal(filteredRules)
					if err == nil {
						rawPermissions["deny"] = denyJSON
					}
				} else {
					// Remove empty deny array
					delete(rawPermissions, "deny")
				}
			}
		}

		// If permissions is empty, remove it entirely
		if len(rawPermissions) > 0 {
			permJSON, err := json.Marshal(rawPermissions)
			if err == nil {
				rawSettings["permissions"] = permJSON
			}
		} else {
			delete(rawSettings, "permissions")
		}
	}

	// Marshal hooks back (preserving unknown hook types)
	if len(rawHooks) > 0 {
		hooksJSON, err := json.Marshal(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawSettings["hooks"] = hooksJSON
	} else {
		delete(rawSettings, "hooks")
	}

	// Write back
	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (c *ClaudeCodeAgent) AreHooksInstalled(ctx context.Context) bool {
	// Use repo root to find .claude directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)
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
	for _, prefix := range entireHookPrefixes {
		if strings.HasPrefix(command, prefix) {
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

// digestSkillDir is the skill directory name for the digest skill.
const digestSkillDir = "entire-digest"

// digestSkillContent is the SKILL.md content for the /entire-digest skill.
const digestSkillContent = `---
name: entire-digest
description: Team highlights newsletter - funniest exchanges, cleverest prompts, and stats from AI coding sessions
user-invocable: true
argument-hint: "what's the team been up to, Bob's funniest highlights, cleverest prompts this week"
---

# Team Activity Digest

Show what the team has been working on by running ` + "`entire digest`" + ` and presenting the highlights inline - like the best moments from a team Slack channel.

## Step 1: Find Entire-Enabled Repos

Before running digest, find which repos have Entire enabled. Run this via Bash:

` + "```bash" + `
find ~ -maxdepth 3 -name ".entire" -type d 2>/dev/null | sed 's|/.entire$||'
` + "```" + `

If the current directory has ` + "`.entire/`" + `, include it too.

If no repos are found, tell the user: "No Entire-enabled repos found. Run ` + "`entire enable`" + ` in your team's project first."

## Step 2: Parse User Intent

Map the user's request to CLI flags:

| User says | Flag |
|-----------|------|
| "last 30 days", "last month" | ` + "`--period 30d`" + ` |
| "today" | ` + "`--period today`" + ` |
| "all time", "everything" | ` + "`--period all`" + ` |
| No period specified | ` + "`--period 7d`" + ` (default) |
| A person's name (e.g., "myk", "sheldon") | ` + "`--author <name>`" + ` |
| "quick", "summary", "stats only" | ` + "`--short`" + ` |

## Step 3: Run Digest

For EACH repo found in Step 1, run via Bash:

` + "```bash" + `
cd [REPO_PATH] && entire digest --format json --no-pager --ai-curate=false [FLAGS]
` + "```" + `

If the command fails with "unknown format", fall back to:

` + "```bash" + `
cd [REPO_PATH] && entire digest --format markdown --no-pager --ai-curate=false [FLAGS]
` + "```" + `

Use a timeout of 30000ms. Always include ` + "`--no-pager --ai-curate=false`" + `.

Skip repos that return "No checkpoints found" - only present repos that have data.

## Step 4: Error Handling

- **"command not found"**: Tell the user: "The ` + "`entire`" + ` CLI is not installed. Install it with: ` + "`brew install entireio/tap/entire`" + `"
- **All repos empty**: Tell the user their team needs to work with AI agents and commit to create checkpoints.

## Step 5: Write the Digest

You are writing a team newsletter with two sections. Present them in this order.

### Understanding the data

**If you got JSON output**, look at the ` + "`top_conversations`" + ` array. Each conversation has:
- ` + "`score`" + ` - a rough interestingness signal, not a quality guarantee. Use it to prioritize scanning, but trust your own editorial judgment over the score
- ` + "`exchanges`" + ` - the actual back-and-forth between human and AI
- ` + "`author`" + `, ` + "`branch`" + `, ` + "`timestamp`" + ` for context

**Important: Prioritize variety.** The top_conversations array may contain near-duplicate prompts that appeared across multiple sessions. Never feature the same prompt twice. Pick at most one item per "theme" - one frustrated moment, one terse prompt, one celebration. Dig deeper in the list for unique moments rather than clustering around the highest scores.

**If you got markdown output**, the prompts are block-quoted under each author, sorted by score (most interesting first). You won't have assistant responses, so focus on the human's words.

### Section 1: This Week's Highlights (3-5 items)

The entertaining, emotional, personality-driven moments.

**What to look for:**
- **Logical contradictions** - Claude says X then does the opposite, and the human catches it. "If they were never committed, why did you commit a fix for them?" These are gold.
- **Escalating exchanges** - Short follow-ups showing mounting frustration: "Really?" then "No." then "Try again." then "STILL WRONG". The CONVERSATION ARC is what's funny, not any single message.
- **Frustrated developer moments** - Exasperation with AI: "Thank you, Captain Obvious", "That's literally what I just said"
- **Beautifully terse prompts** - A one-word prompt like "ls" or "Word!" or just "no" that captures a whole vibe
- **Celebration moments** - "IT WORKS!", "Finally!", "Ship it!" after a long struggle
- **Same issue hitting multiple people** - Two team members independently developing the same coping mechanism
- **The human's personality showing** - Humor, brevity, sarcasm, joy
- **"No but" redirections** - When Claude answers the wrong question confidently and the human redirects with minimal words

### Section 2: Cleverest Prompts (2-3 items)

The smartest, most creative, or most inventive uses of AI tools. This section celebrates craft, not emotion.

**What to look for:**
- **Creative problem-solving** - Unusual approaches, clever workarounds, using AI in unexpected ways
- **Meta/recursive prompts** - Using Claude to debug Claude, self-referential humor, prompts about prompting
- **Elegant tool mastery** - Prompts that show deep knowledge of what the AI can do
- **Surprising results** - Prompts that made the AI do something non-obvious or impressive
- **Inventive workflows** - Chaining tools creatively, using skills in unexpected combinations

**How these differ from Highlights:** Highlights are about personality and emotion (the human reacting). Cleverest Prompts are about craft and creativity (the human thinking).

### What to skip (both sections)

- Long copy-pasted specs or requirements (boring context dumps)
- Routine "fix the tests" without interesting context
- Auto-generated session continuations
- Normal productive back-and-forth (good work but not entertaining or clever)

### How to write each item

1. **Lead with the best quote as a title** - Actual words someone typed, in quotes
2. **Name the person** and give context (branch, date)
3. **Quote the actual exchange** if you have it - the back-and-forth is the good stuff
4. **Add 2-3 sentences of editorial commentary** explaining why this moment is great

Style: **Affectionate and celebratory, never mocking.** Think "inside joke among friends." Quote actual words, don't paraphrase.

For Highlights, commentary should have voice: "Classic frustrated-developer-with-AI moment", "the patience of a saint, tested."

For Cleverest Prompts, appreciate the craft: "The prompt equivalent of a hole-in-one", "Three words that replaced a 200-line config", "Galaxy-brain move."

### After the two sections

Show a brief stats summary (sessions, prompts, files, tokens per author) but keep it secondary. The highlights and clever prompts ARE the digest - stats are just context.
`

// InstallSkills installs Claude Code skills (e.g., /digest).
// If force is true, overwrites existing skill files.
func (c *ClaudeCodeAgent) InstallSkills(_ bool, force bool) error {
	repoRoot, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	skillDir := filepath.Join(repoRoot, ".claude", "skills", digestSkillDir)
	skillPath := filepath.Join(skillDir, "SKILL.md")

	// Skip if exists and not forcing
	if !force {
		if _, err := os.Stat(skillPath); err == nil {
			return nil
		}
	}

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return fmt.Errorf("failed to create skill directory: %w", err)
	}

	return os.WriteFile(skillPath, []byte(digestSkillContent), 0o644) //nolint:gosec // skill files need to be readable
}

// UninstallSkills removes installed skill files.
func (c *ClaudeCodeAgent) UninstallSkills() error {
	repoRoot, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		repoRoot = "."
	}

	skillDir := filepath.Join(repoRoot, ".claude", "skills", digestSkillDir)
	if err := os.RemoveAll(skillDir); err != nil {
		return fmt.Errorf("failed to remove digest skill: %w", err)
	}
	return nil
}
