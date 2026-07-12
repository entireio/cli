package codex

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

// HooksFileName is the hooks config file used by Codex.
const HooksFileName = "hooks.json"

// entireHookPrefixes identifies Entire hook commands. The "go run" prefix is
// retained so hooks installed by older versions are still recognized.
var entireHookPrefixes = []string{
	"entire ",
	agent.LocalDevHookScript + " ",
	`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go `,
}

// InstallHooks installs Codex hooks in .codex/hooks.json.
func (c *CodexAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)

	// Read existing hooks.json if present
	var rawHooks map[string]json.RawMessage
	existingData, readErr := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if readErr == nil {
		var hooksFile map[string]json.RawMessage
		if err := json.Unmarshal(existingData, &hooksFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing hooks.json: %w", err)
		}
		if hooksRaw, ok := hooksFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in hooks.json: %w", err)
			}
		}
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse event types we manage
	var sessionStart, userPromptSubmit, stop, postToolUse []MatcherGroup
	if err := parseHookType(rawHooks, "SessionStart", &sessionStart); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "Stop", &stop); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "PostToolUse", &postToolUse); err != nil {
		return 0, err
	}

	if force {
		sessionStart = removeEntireHooks(sessionStart)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		stop = removeEntireHooks(stop)
		postToolUse = removeEntireHooks(postToolUse)
	}

	// Build hook commands
	var cmdPrefix string
	if localDev {
		cmdPrefix = agent.LocalDevHookScript + " hooks codex "
	} else {
		cmdPrefix = "entire hooks codex "
	}
	sessionStartCmd := cmdPrefix + "session-start"
	useWindowsProductionHooks := agent.UseWindowsProductionHooks(ctx, localDev)
	if !localDev {
		sessionStartCmd = agent.WrapProductionJSONWarningHookCommandForOS(sessionStartCmd, agent.WarningFormatSingleLine, useWindowsProductionHooks)
	}
	userPromptSubmitCmd := cmdPrefix + "user-prompt-submit"
	stopCmd := cmdPrefix + "stop"
	postToolUseCmd := cmdPrefix + "post-tool-use"
	if !localDev {
		userPromptSubmitCmd = agent.WrapProductionSilentHookCommandForOS(userPromptSubmitCmd, useWindowsProductionHooks)
		stopCmd = agent.WrapProductionSilentHookCommandForOS(stopCmd, useWindowsProductionHooks)
		postToolUseCmd = agent.WrapProductionSilentHookCommandForOS(postToolUseCmd, useWindowsProductionHooks)
	}

	count := 0

	if updated, changed := syncHookCommand(sessionStart, sessionStartCmd); changed {
		sessionStart = updated
		count++
	}
	if updated, changed := syncHookCommand(userPromptSubmit, userPromptSubmitCmd); changed {
		userPromptSubmit = updated
		count++
	}
	if updated, changed := syncHookCommand(stop, stopCmd); changed {
		stop = updated
		count++
	}
	if updated, changed := syncHookCommand(postToolUse, postToolUseCmd); changed {
		postToolUse = updated
		count++
	}

	if count == 0 {
		// Still ensure the feature flag is configured even if hooks
		// were already present (e.g., manually installed).
		if err := ensureProjectFeatureEnabled(repoRoot); err != nil {
			return 0, fmt.Errorf("failed to enable codex_hooks feature: %w", err)
		}
		return 0, nil
	}

	// Marshal modified types back
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Preserve existing top-level keys (e.g., $schema) by reusing the parsed file
	topLevel := make(map[string]json.RawMessage)
	if readErr == nil {
		// Re-parse the original file to preserve all top-level keys
		_ = json.Unmarshal(existingData, &topLevel) //nolint:errcheck // best-effort preservation
	}
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	topLevel["hooks"] = hooksJSON

	// Write to file
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .codex directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks.json: %w", err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write hooks.json: %w", err)
	}

	// Enable the codex_hooks feature in the project-level .codex/config.toml.
	// This keeps the feature flag per-repo rather than global.
	if err := ensureProjectFeatureEnabled(repoRoot); err != nil {
		return count, fmt.Errorf("failed to enable codex_hooks feature: %w", err)
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from Codex hooks.json.
func (c *CodexAgent) UninstallHooks(ctx context.Context) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		return nil //nolint:nilerr // No hooks.json means nothing to uninstall
	}

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return fmt.Errorf("failed to parse hooks.json: %w", err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks: %w", err)
		}
	}
	if rawHooks == nil {
		return nil
	}

	var sessionStart, userPromptSubmit, stop, postToolUse []MatcherGroup
	if err := parseHookType(rawHooks, "SessionStart", &sessionStart); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "Stop", &stop); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "PostToolUse", &postToolUse); err != nil {
		return err
	}

	sessionStart = removeEntireHooks(sessionStart)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	stop = removeEntireHooks(stop)
	postToolUse = removeEntireHooks(postToolUse)

	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		topLevel["hooks"] = hooksJSON
	} else {
		delete(topLevel, "hooks")
	}

	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write hooks.json: %w", err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed in Codex hooks.json.
func (c *CodexAgent) AreHooksInstalled(ctx context.Context) bool {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		return false
	}

	var hooksFile HooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		return false
	}

	return hasEntireHook(hooksFile.Hooks.SessionStart) &&
		hasEntireHook(hooksFile.Hooks.UserPromptSubmit) &&
		hasEntireHook(hooksFile.Hooks.Stop) &&
		hasEntireHook(hooksFile.Hooks.PostToolUse)
}

// --- Helpers ---

func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]MatcherGroup) error {
	if data, ok := rawHooks[hookType]; ok {
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("failed to parse %s hooks: %w", hookType, err)
		}
	}
	return nil
}

func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, groups []MatcherGroup) {
	if len(groups) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(groups)
	if err != nil {
		return
	}
	rawHooks[hookType] = data
}

func hookCommandExists(groups []MatcherGroup, command string) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func syncHookCommand(groups []MatcherGroup, command string) ([]MatcherGroup, bool) {
	if hookCommandExists(groups, command) {
		return groups, false
	}
	if hasEntireHook(groups) {
		groups = removeEntireHooks(groups)
	}
	return addHook(groups, command), true
}

func addHook(groups []MatcherGroup, command string) []MatcherGroup {
	entry := HookEntry{
		Type:    "command",
		Command: command,
		Timeout: 30,
	}

	// Add to an existing group with null matcher, or create a new one
	for i, group := range groups {
		if group.Matcher == nil {
			groups[i].Hooks = append(groups[i].Hooks, entry)
			return groups
		}
	}
	return append(groups, MatcherGroup{
		Matcher: nil,
		Hooks:   []HookEntry{entry},
	})
}

func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command, entireHookPrefixes)
}

func hasEntireHook(groups []MatcherGroup) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func removeEntireHooks(groups []MatcherGroup) []MatcherGroup {
	result := make([]MatcherGroup, 0, len(groups))
	for _, group := range groups {
		filtered := make([]HookEntry, 0, len(group.Hooks))
		for _, hook := range group.Hooks {
			if !isEntireHook(hook.Command) {
				filtered = append(filtered, hook)
			}
		}
		if len(filtered) > 0 {
			group.Hooks = filtered
			result = append(result, group)
		}
	}
	return result
}

// configFileName is the Codex config file name.
const configFileName = "config.toml"

// featureLine is the TOML line that enables the hooks feature. The flag was
// renamed from `codex_hooks` to `hooks` in Codex 0.129.0; the old name is
// still accepted as a legacy alias but emits a deprecation warning at
// every startup. ensureProjectFeatureEnabled rewrites the legacy form when
// it sees it.
const (
	featureLine       = "hooks = true"
	legacyFeatureLine = "codex_hooks = true"
)

// ensureProjectFeatureEnabled enables the hooks feature for repoRoot.
//
// Normally that means writing features.hooks = true to the project-level
// .codex/config.toml, keeping the flag scoped per-repo. But when repoRoot
// lives inside <CODEX_HOME>/agents (a reserved tree Codex recursively scans
// for custom agent-role TOML files), a project-local .codex/config.toml
// there gets misinterpreted as a malformed agent role definition and
// triggers a Codex startup warning (entireio/cli#842). In that case the
// flag is scoped to this project via a [projects."<repoRoot>".features]
// table in the global config.toml instead, and any stale project-local
// config.toml left behind by older entire versions is cleaned up.
func ensureProjectFeatureEnabled(repoRoot string) error {
	codexHome, err := resolveCodexHome()
	if err == nil && isUnderCodexAgentsDir(repoRoot, codexHome) {
		return ensureScopedProjectFeatureEnabled(codexHome, repoRoot)
	}
	return ensureLocalProjectFeatureEnabled(repoRoot)
}

// isUnderCodexAgentsDir reports whether repoRoot lives inside
// <codexHome>/agents, the reserved tree Codex recursively scans for custom
// agent-role TOML files (see developers.openai.com/codex/subagents).
func isUnderCodexAgentsDir(repoRoot, codexHome string) bool {
	agentsDir := filepath.Join(codexHome, "agents")
	rel, err := filepath.Rel(agentsDir, repoRoot)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// ensureLocalProjectFeatureEnabled writes features.hooks = true to the
// project-level .codex/config.toml. This keeps the feature flag per-repo.
// Replaces the deprecated codex_hooks = true line if it's present.
func ensureLocalProjectFeatureEnabled(repoRoot string) error {
	configPath := filepath.Join(repoRoot, ".codex", configFileName)

	data, err := os.ReadFile(configPath) //nolint:gosec // path constructed from repo root
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read config.toml: %w", err)
	}

	content := string(data)
	hasNew := containsFeatureLine(content, featureLine)
	hasLegacy := containsFeatureLine(content, legacyFeatureLine)
	switch {
	case hasNew && hasLegacy:
		content = stripLegacyFeatureLine(content)
	case hasNew:
		return nil
	case hasLegacy:
		content = strings.Replace(content, legacyFeatureLine, featureLine, 1)
	case strings.Contains(content, "[features]"):
		content = strings.Replace(content, "[features]", "[features]\n"+featureLine, 1)
	default:
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += "\n[features]\n" + featureLine + "\n"
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		return fmt.Errorf("failed to create .codex directory: %w", err)
	}
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil { //nolint:gosec // path constructed from repo root
		return fmt.Errorf("failed to write config.toml: %w", err)
	}
	return nil
}

// containsFeatureLine checks for an exact line match. A plain
// strings.Contains is wrong because "hooks = true" is a substring of
// "codex_hooks = true" — without the line-boundary anchor we'd treat the
// legacy form as if the new form was already present.
func containsFeatureLine(content, line string) bool {
	for _, raw := range strings.Split(content, "\n") {
		if strings.TrimSpace(raw) == line {
			return true
		}
	}
	return false
}

// stripLegacyFeatureLine removes the deprecated `codex_hooks = true` line
// from a TOML config string, dropping a trailing blank line so the file
// stays tidy. The new `hooks = true` is added separately by the caller.
func stripLegacyFeatureLine(content string) string {
	idx := strings.Index(content, legacyFeatureLine)
	if idx < 0 {
		return content
	}
	end := idx + len(legacyFeatureLine)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:idx] + content[end:]
}

// ensureScopedProjectFeatureEnabled writes features.hooks = true scoped to
// repoRoot via a [projects."<repoRoot>".features] table in the global
// <codexHome>/config.toml, instead of a project-local .codex/config.toml.
// This is used when repoRoot falls inside Codex's reserved agents tree; see
// ensureProjectFeatureEnabled. It never touches other projects' sections or
// unrelated keys in the global config, and cleans up any stale
// project-local config.toml an older entire version left behind.
func ensureScopedProjectFeatureEnabled(codexHome, repoRoot string) error {
	configPath := filepath.Join(codexHome, configFileName)

	data, err := os.ReadFile(configPath) //nolint:gosec // path resolved from CODEX_HOME or HOME
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read global config.toml: %w", err)
	}

	updated, changed := ensureLineInSection(string(data), scopedFeaturesHeader(repoRoot), featureLine)
	if changed {
		if err := os.MkdirAll(codexHome, 0o750); err != nil {
			return fmt.Errorf("failed to create Codex home directory: %w", err)
		}
		if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil { //nolint:gosec // path resolved from CODEX_HOME or HOME
			return fmt.Errorf("failed to write global config.toml: %w", err)
		}
	}

	return cleanupStaleReservedTreeConfig(repoRoot)
}

// scopedFeaturesHeader returns the TOML table header used to scope the
// hooks feature to a single project inside the global config.toml, e.g.
//
//	[projects."/Users/x/.codex/agents/repos/project".features]
func scopedFeaturesHeader(repoRoot string) string {
	return "[projects." + tomlQuoteString(repoRoot) + ".features]"
}

// tomlQuoteString renders s as a TOML basic (double-quoted) string.
func tomlQuoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ensureLineInSection inserts line as the first line under the exact TOML
// table header in content, creating the table (appended at the end of the
// file) if it doesn't already exist. It reports the updated content and
// whether a change was made. Like containsFeatureLine, this is a line-based
// edit rather than a full TOML parse: it only recognizes an unindented,
// exact header line, and a table's body ends at the next line starting
// with "[". That's sufficient for the narrow shape this package writes and
// avoids touching content it doesn't understand.
func ensureLineInSection(content, header, line string) (string, bool) {
	lines := strings.Split(content, "\n")

	headerIdx := -1
	for i, raw := range lines {
		if strings.TrimSpace(raw) == header {
			headerIdx = i
			break
		}
	}

	if headerIdx == -1 {
		trimmed := strings.TrimRight(content, "\n")
		if trimmed == "" {
			return header + "\n" + line + "\n", true
		}
		return trimmed + "\n\n" + header + "\n" + line + "\n", true
	}

	end := len(lines)
	for i := headerIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == line {
			return content, false
		}
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}

	updated := make([]string, 0, len(lines)+1)
	updated = append(updated, lines[:headerIdx+1]...)
	updated = append(updated, line)
	updated = append(updated, lines[headerIdx+1:end]...)
	updated = append(updated, lines[end:]...)
	return strings.Join(updated, "\n"), true
}

// cleanupStaleReservedTreeConfig removes a project-local .codex/config.toml
// left behind by an older entire version when repoRoot lives inside
// Codex's reserved agents tree. Codex recursively scans that tree for
// agent-role TOML files, so a leftover config.toml there keeps triggering
// a "malformed agent role definition" warning (entireio/cli#842) even
// after upgrading entire. Only removes the file when, after stripping the
// exact feature-flag lines and an empty [features] header this package
// writes, nothing else remains — a file carrying unrelated user content is
// left alone.
func cleanupStaleReservedTreeConfig(repoRoot string) error {
	configPath := filepath.Join(repoRoot, ".codex", configFileName)

	data, err := os.ReadFile(configPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read stale config.toml: %w", err)
	}

	remainder := string(data)
	for _, known := range []string{legacyFeatureLine, featureLine, "[features]"} {
		remainder = strings.ReplaceAll(remainder, known+"\n", "")
		remainder = strings.ReplaceAll(remainder, known, "")
	}
	if strings.TrimSpace(remainder) != "" {
		return nil
	}

	if err := os.Remove(configPath); err != nil {
		return fmt.Errorf("failed to remove stale config.toml: %w", err)
	}
	return nil
}
