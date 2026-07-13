package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
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
	if rel == "." {
		return true
	}
	// rel escapes agentsDir only when it is exactly ".." or begins with
	// "../". A plain strings.HasPrefix(rel, "..") check would wrongly reject
	// a directory literally named "..foo" that really does live under
	// agentsDir.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
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
	if err := writeScopedFeatureToGlobalConfig(codexHome, repoRoot); err != nil {
		return err
	}
	return cleanupStaleReservedTreeConfig(repoRoot)
}

// writeScopedFeatureToGlobalConfig merges the scoped
// [projects."<repoRoot>".features] hooks flag into the global config.toml.
// The global file is shared by every repo that enables hooks from inside the
// reserved agents tree and is read by Codex at startup, so the
// read-modify-write is serialized under a cross-process advisory lock and
// swapped in atomically. Two concurrent `entire enable` runs therefore can
// neither lose each other's edits (a plain read-modify-write would let the
// second writer clobber the first) nor expose a truncated half-written file
// to a concurrent reader.
func writeScopedFeatureToGlobalConfig(codexHome, repoRoot string) error {
	if err := os.MkdirAll(codexHome, 0o750); err != nil {
		return fmt.Errorf("failed to create Codex home directory: %w", err)
	}

	configPath := filepath.Join(codexHome, configFileName)
	release, err := flock.Acquire(configPath + ".lock")
	if err != nil {
		return fmt.Errorf("failed to lock global config.toml: %w", err)
	}
	defer release()

	data, err := os.ReadFile(configPath) //nolint:gosec // path resolved from CODEX_HOME or HOME
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read global config.toml: %w", err)
	}

	updated, changed := ensureLineInSection(string(data), scopedFeaturesHeader(repoRoot), featureLine)
	if !changed {
		return nil
	}
	if err := jsonutil.WriteFileAtomic(configPath, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("failed to write global config.toml: %w", err)
	}
	return nil
}

// scopedFeaturesHeader returns the TOML table header used to scope the
// hooks feature to a single project inside the global config.toml, e.g.
//
//	[projects."/Users/x/.codex/agents/repos/project".features]
func scopedFeaturesHeader(repoRoot string) string {
	return "[projects." + tomlQuoteString(repoRoot) + ".features]"
}

// tomlQuoteString renders s as a TOML basic (double-quoted) string, escaping
// every character TOML requires so the result is always a single, valid,
// self-contained key. This is security-critical: repoRoot is an
// attacker-influenceable filesystem path that gets embedded into the shared
// global config.toml. A path containing a quote, backslash, newline, or other
// control character must not be able to terminate the quoted key early and
// inject arbitrary TOML — nor split the header across physical lines, which
// would also defeat the line-based idempotency check in ensureLineInSection.
// Unicode scalar values >= U+0020 (other than " and \) are valid literally in
// a TOML basic string and are emitted as-is.
func tomlQuoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ensureLineInSection inserts line as the first line under the exact TOML
// table header in content, creating the table (appended at the end of the
// file) if it doesn't already exist. It reports the updated content and
// whether a change was made. Like containsFeatureLine, this is a line-based
// edit rather than a full TOML parse: it matches a header line by exact
// content after trimming surrounding whitespace (so an indented header line
// also matches), and a table's body ends at the next line starting with "[".
// That's sufficient for the narrow shape this package writes and avoids
// touching content it doesn't understand.
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
// after upgrading entire. Only removes the file when every non-blank line is
// one of the exact feature-flag lines or the [features] header this package
// writes (see isEntireManagedLocalConfig) — a file carrying any unrelated
// user content is left alone.
func cleanupStaleReservedTreeConfig(repoRoot string) error {
	configPath := filepath.Join(repoRoot, ".codex", configFileName)

	data, err := os.ReadFile(configPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read stale config.toml: %w", err)
	}

	if !isEntireManagedLocalConfig(string(data)) {
		return nil
	}
	if err := os.Remove(configPath); err != nil {
		return fmt.Errorf("failed to remove stale config.toml: %w", err)
	}
	return nil
}

// isEntireManagedLocalConfig reports whether content consists of nothing but
// the [features] header and feature-flag lines this package writes (plus blank
// lines), with at least one such managed line present. Any other non-blank
// line — a user's own setting or a comment — makes the file unmanaged, so
// cleanup leaves it untouched.
//
// The check is line-anchored on purpose. An earlier substring scan could strip
// our tokens out of the middle of unrelated content (`webhooks = true`
// contains `hooks = true`; a value could contain `[features]`) and, in the
// worst case, collapse a file we never wrote to whitespace and delete it. A
// whole-line match cannot mistake a user's line for ours.
func isEntireManagedLocalConfig(content string) bool {
	managed := map[string]bool{
		"[features]":      true,
		featureLine:       true,
		legacyFeatureLine: true,
	}
	sawManaged := false
	for _, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if !managed[trimmed] {
			return false
		}
		sawManaged = true
	}
	return sawManaged
}
