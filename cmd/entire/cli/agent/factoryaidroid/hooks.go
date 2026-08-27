package factoryaidroid

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure FactoryAIDroidAgent implements HookSupport
var _ agent.HookSupport = (*FactoryAIDroidAgent)(nil)

// Factory AI Droid hook names - these become subcommands under `entire hooks factoryai-droid`
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNamePreToolUse       = "pre-tool-use"
	HookNamePostToolUse      = "post-tool-use"
	HookNameSubagentStop     = "subagent-stop"
	HookNamePreCompact       = "pre-compact"
	HookNameNotification     = "notification"
)

// FactorySettingsFileName is the settings file used by Factory AI Droid.
// This is Factory-specific and not shared with other agents.
const FactorySettingsFileName = "settings.json"

// metadataDenyRule blocks Factory Droid from reading Entire session metadata
const metadataDenyRule = "Read(./.entire/metadata/**)"

// InstallHooks installs Factory AI Droid hooks in .factory/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
//
//nolint:maintidx // Hook installation is intentionally centralized here; splitting it further would add churn for a config-assembly path.
func (f *FactoryAIDroidAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	// Use repo root instead of CWD to find .factory directory
	// This ensures hooks are installed correctly when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".factory", FactorySettingsFileName)

	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage

	// rawPermissions preserves unknown permission fields (e.g., "ask")
	var rawPermissions map[string]json.RawMessage

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from cwd + fixed path
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
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse, preCompact []FactoryHookMatcher
	for _, parsed := range []struct {
		hookType string
		target   *[]FactoryHookMatcher
	}{
		{"SessionStart", &sessionStart},
		{"SessionEnd", &sessionEnd},
		{"Stop", &stop},
		{"UserPromptSubmit", &userPromptSubmit},
		{"PreToolUse", &preToolUse},
		{"PostToolUse", &postToolUse},
		{"PreCompact", &preCompact},
	} {
		if err := parseHookType(rawHooks, parsed.hookType, parsed.target); err != nil {
			return 0, err
		}
	}

	// If force is true, remove all existing Entire hooks first
	if force {
		sessionStart = removeEntireHooks(sessionStart)
		sessionEnd = removeEntireHooks(sessionEnd)
		stop = removeEntireHooks(stop)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		preToolUse = removeEntireHooks(preToolUse)
		postToolUse = removeEntireHooks(postToolUse)
		preCompact = removeEntireHooks(preCompact)
	}

	// Define hook commands
	sessionStartCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid session-start")
	sessionEndCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid session-end")
	stopCmd := agent.WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid stop", agent.WarningFormatSingleLine)
	userPromptSubmitCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid user-prompt-submit")
	preTaskCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid pre-tool-use")
	postTaskCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid post-tool-use")
	preCompactCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid pre-compact")

	// Drop Entire hooks left by older versions before adding the current ones,
	// so a stale command (e.g. the removed local-dev launcher) does not survive
	// alongside them. Unconditional: a plain `entire enable` must migrate too.
	staleDropped := false
	drop := func(matchers []FactoryHookMatcher, want ...string) []FactoryHookMatcher {
		out, dropped := dropStaleEntireHooks(matchers, want...)
		if dropped {
			staleDropped = true
		}
		return out
	}
	sessionStart = drop(sessionStart, sessionStartCmd, userPromptSubmitCmd)
	sessionEnd = drop(sessionEnd, sessionEndCmd)
	stop = drop(stop, stopCmd)
	userPromptSubmit = drop(userPromptSubmit, userPromptSubmitCmd)
	preToolUse = drop(preToolUse, preTaskCmd)
	postToolUse = drop(postToolUse, postTaskCmd)
	preCompact = drop(preCompact, preCompactCmd)

	count := 0

	// Add hooks if they don't exist
	if !hookCommandExists(sessionStart, sessionStartCmd) {
		sessionStart = addHookToMatcher(sessionStart, "", sessionStartCmd)
		count++
	}
	// Also install user-prompt-submit on SessionStart to ensure TurnStart fires
	// even when UserPromptSubmit doesn't (e.g., droid exec mode).
	// The user-prompt-submit handler gracefully handles SessionStart's stdin format
	// (userPromptSubmitRaw is a superset of sessionInfoRaw; Prompt defaults to "").
	if !hookCommandExists(sessionStart, userPromptSubmitCmd) {
		sessionStart = addHookToMatcher(sessionStart, "", userPromptSubmitCmd)
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
	if !hookCommandExists(preCompact, preCompactCmd) {
		preCompact = addHookToMatcher(preCompact, "", preCompactCmd)
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

	// staleDropped forces a write even when nothing was added: a file holding
	// both a stale and a current hook adds nothing, and returning early here
	// would leave the stale hook on disk.
	if count == 0 && !permissionsChanged && !staleDropped {
		return 0, nil // All hooks and permissions already installed
	}

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)
	marshalHookType(rawHooks, "PreCompact", preCompact)

	// Marshal hooks and update raw settings
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	// Marshal permissions and update raw settings
	permJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawPermissions)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal permissions: %w", err)
	}
	rawSettings["permissions"] = permJSON

	// Write back to file
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .factory directory: %w", err)
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
// A hook type that will not parse is reported rather than treated as empty: to
// removal that is the difference between "Entire owns nothing here" and "we
// could not tell", and the two must not be collapsed.
func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]FactoryHookMatcher) error {
	data, ok := rawHooks[hookType]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("failed to parse %s hooks: %w", hookType, err)
	}
	return nil
}

// marshalHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []FactoryHookMatcher) {
	if len(matchers) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(matchers)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[hookType] = data
}

// managedHookTypes is every .factory/settings.json key Entire installs hooks
// under. Removal walks this list and detection is derived from removal, so a
// hook type cannot be stripped by one and missed by the other.
//
// InstallHooks keeps its own table because it also carries each type's command
// and matcher; TestAreHooksInstalledMatchesUninstallHooks pins the two together.
var managedHookTypes = []string{
	"SessionStart",
	"SessionEnd",
	"Stop",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"PreCompact",
}

// factorySettingsPath resolves .factory/settings.json for the current repo, so
// install, detection and removal all look at the same file.
func factorySettingsPath(ctx context.Context) string {
	return agent.HookConfigPath(ctx, ".factory", FactorySettingsFileName)
}

// UninstallHooks removes Entire's hooks and its metadata deny rule from Factory
// AI Droid settings.
func (f *FactoryAIDroidAgent) UninstallHooks(ctx context.Context) error {
	if err := agent.RemoveHookArtifacts(factorySettingsPath(ctx), removeEntireArtifacts); err != nil {
		return fmt.Errorf("remove Factory AI Droid hooks: %w", err)
	}
	return nil
}

// AreHooksInstalled reports whether Entire owns anything in Factory AI Droid's
// settings, by asking whether UninstallHooks would strip anything. Both answers
// come from one description of what Entire owns here, so detection cannot be
// narrower than removal — it used to look at the Stop hook alone while removal
// covered seven hook types and the metadata deny rule.
func (f *FactoryAIDroidAgent) AreHooksInstalled(ctx context.Context) bool {
	return agent.HookArtifactsInstalled(ctx, string(f.Name()), factorySettingsPath(ctx), removeEntireArtifacts)
}

// removeEntireArtifacts implements agent.HookArtifactRemoval for Factory AI
// Droid: Entire's hooks under every managed hook type, plus the metadata deny
// rule it adds to permissions. Everything else in the file survives, which is
// why the transforms work on the raw JSON maps rather than a typed struct.
func removeEntireArtifacts(data []byte) ([]byte, bool, error) {
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return nil, false, fmt.Errorf("failed to parse settings.json: %w", err)
	}

	// rawHooks preserves unknown hook types.
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return nil, false, fmt.Errorf("failed to parse hooks: %w", err)
		}
	}

	changed := false
	for _, hookType := range managedHookTypes {
		var matchers []FactoryHookMatcher
		if err := parseHookType(rawHooks, hookType, &matchers); err != nil {
			return nil, false, err
		}
		if !hasEntireHook(matchers) {
			continue
		}
		changed = true
		marshalHookType(rawHooks, hookType, removeEntireHooks(matchers))
	}

	denyRuleRemoved, err := agent.RemoveDenyRule(rawSettings, metadataDenyRule)
	if err != nil {
		return nil, false, fmt.Errorf("remove metadata deny rule: %w", err)
	}
	changed = changed || denyRuleRemoved

	// Nothing of Entire's was here. Report that rather than rewriting a file we
	// have no artifacts in: removal sweeps every agent, including ones the user
	// never enabled.
	if !changed {
		return nil, false, nil
	}

	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return nil, false, fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawSettings["hooks"] = hooksJSON
	} else {
		delete(rawSettings, "hooks")
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal settings: %w", err)
	}
	return output, true, nil
}

// Helper functions for hook management

func hookCommandExists(matchers []FactoryHookMatcher, command string) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func hasEntireHook(matchers []FactoryHookMatcher) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func hookCommandExistsWithMatcher(matchers []FactoryHookMatcher, matcherName, command string) bool {
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

func addHookToMatcher(matchers []FactoryHookMatcher, matcherName, command string) []FactoryHookMatcher {
	entry := FactoryHookEntry{Type: "command", Command: command}
	for i := range matchers {
		if matchers[i].Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}
	return append(matchers, FactoryHookMatcher{Matcher: matcherName, Hooks: []FactoryHookEntry{entry}})
}

// isEntireHook checks if a command is an Entire hook
func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

// dropStaleEntireHooks removes Entire-owned hooks whose command is not one of
// want, per matcher, pruning matchers left with no hooks. want is a set because
// one hook list can hold several Entire commands (SessionStart carries both
// session-start and user-prompt-submit). See agent.DropStaleManagedHooks for why
// this runs on every install and why the dropped flag matters.
func dropStaleEntireHooks(matchers []FactoryHookMatcher, want ...string) ([]FactoryHookMatcher, bool) {
	result := make([]FactoryHookMatcher, 0, len(matchers))
	dropped := false
	for _, matcher := range matchers {
		kept, d := agent.DropStaleManagedHooks(matcher.Hooks, hookEntryCommand, want)
		if d {
			dropped = true
		}
		if len(kept) > 0 {
			matcher.Hooks = kept
			result = append(result, matcher)
		}
	}
	if !dropped {
		return matchers, false
	}
	return result, true
}

// hookEntryCommand reads the command off a hook entry for the shared helpers.
func hookEntryCommand(e FactoryHookEntry) string { return e.Command }

// removeEntireHooks removes all Entire hooks from a list of matchers (for simple
// hooks like Stop). It is dropStaleEntireHooks with an empty want set: nothing is
// wanted, so every managed hook is stale.
func removeEntireHooks(matchers []FactoryHookMatcher) []FactoryHookMatcher {
	out, _ := dropStaleEntireHooks(matchers)
	return out
}
