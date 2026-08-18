package codex

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

// HooksFileName is the hooks config file used by Codex.
const HooksFileName = "hooks.json"

// defaultHookTimeoutSec is the timeout Entire configures for Codex hooks that
// run between turns, where Codex allows up to its standard 600s.
const defaultHookTimeoutSec = 30

// managedHook describes one hooks.json event Entire owns. Keeping the event
// key, verb, timeout and production wrapper together means adding or removing
// an event is a single table edit rather than parallel edits in InstallHooks,
// UninstallHooks and AreHooksInstalled.
type managedHook struct {
	event   string // hooks.json key
	verb    string // `entire hooks codex <verb>`
	timeout int
	wrap    func(cmd string, windows bool) string

	// core marks the events whose absence means Codex was never enabled in this
	// repo, as opposed to enabled against an older release that installed fewer
	// events. Only these gate AreHooksInstalled — see the comment there.
	core bool
}

// managedHooks is the full set of Codex events Entire installs.
var managedHooks = []managedHook{
	{event: "SessionStart", verb: HookNameSessionStart, timeout: defaultHookTimeoutSec, core: true, wrap: func(cmd string, windows bool) string {
		return agent.WrapProductionJSONWarningHookCommandForOS(cmd, agent.WarningFormatSingleLine, windows)
	}},
	// SessionEnd is the one event Codex clamps: it caps handlers at
	// SESSION_END_MAX_TIMEOUT_SEC and warns at every startup when a config asks
	// for more, so it is installed at exactly the ceiling. See SessionEndTimeoutSec.
	//
	// Not core: it postdates the four events below, so requiring it would
	// un-enable Codex for everyone who enabled it before this release.
	{event: "SessionEnd", verb: HookNameSessionEnd, timeout: SessionEndTimeoutSec, wrap: agent.WrapProductionSilentHookCommandForOS},
	{event: "UserPromptSubmit", verb: HookNameUserPromptSubmit, timeout: defaultHookTimeoutSec, core: true, wrap: agent.WrapProductionSilentHookCommandForOS},
	{event: "Stop", verb: HookNameStop, timeout: defaultHookTimeoutSec, core: true, wrap: agent.WrapProductionSilentHookCommandForOS},
	{event: "PostToolUse", verb: HookNamePostToolUse, timeout: defaultHookTimeoutSec, core: true, wrap: agent.WrapProductionSilentHookCommandForOS},
}

// InstallHooks installs Codex hooks in .codex/hooks.json.
func (c *CodexAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
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

	const cmdPrefix = "entire hooks codex "
	useWindowsProductionHooks := agent.UseWindowsProductionHooks(ctx)

	count := 0
	updated := make([][]MatcherGroup, len(managedHooks))
	for i, h := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, h.event, &groups); err != nil {
			return 0, err
		}
		if force {
			groups = removeEntireHooks(groups)
		}
		hookCmd := h.wrap(cmdPrefix+h.verb, useWindowsProductionHooks)
		if synced, changed := syncHookCommand(groups, hookCmd, h.timeout); changed {
			groups = synced
			count++
		}
		updated[i] = groups
	}

	if count == 0 {
		return 0, nil
	}

	for i, h := range managedHooks {
		marshalHookType(rawHooks, h.event, updated[i])
	}

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

	// No .codex/config.toml is written: hooks are enabled by default in
	// Codex (since 0.124.0), and a TOML file inside Codex's reserved
	// <CODEX_HOME>/agents tree would be rejected by its agent-role scanner
	// at every startup (entireio/cli#842). A leftover config.toml written
	// by an older entire version must be removed manually.
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

	for _, h := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, h.event, &groups); err != nil {
			return err
		}
		marshalHookType(rawHooks, h.event, removeEntireHooks(groups))
	}

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

// AreHooksInstalled reports whether Codex is wired up to Entire in this repo.
//
// It requires only the core events, not everything InstallHooks writes today.
// The two questions are different: this one decides whether Codex is listed as
// an installed agent (`entire status`, the review and investigate pickers), and
// answering it with the full set would drop Codex out of all of them the moment
// a release adds an event — every existing install predates the addition. Drift
// against today's set is MissingEntireHooks' job, which `entire doctor` reports
// with the fix (`entire enable`).
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

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return false
	}
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return false
		}
	}

	for _, h := range managedHooks {
		if !h.core {
			continue
		}
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, h.event, &groups); err != nil {
			return false
		}
		if !hasEntireHook(groups) {
			return false
		}
	}
	return true
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

// hookCommandExists reports whether the exact command is already configured
// with the timeout we want. The timeout is part of the match so an upgrade
// rewrites a hook installed by an older Entire with a different budget —
// notably SessionEnd, where a leftover 30s makes Codex print a clamping warning
// at every startup.
func hookCommandExists(groups []MatcherGroup, command string, timeoutSec int) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if hook.Command == command && hook.Timeout == timeoutSec {
				return true
			}
		}
	}
	return false
}

// syncHookCommand ensures groups contains exactly the given Entire hook command
// at the given timeout, and no other Entire-owned entry, reporting whether the
// config changed.
//
// Stale entries are dropped even when command is already present. Checking
// presence first (as this did before) left a hook written by an older version
// sitting next to the current one, so both fired — for the removed local-dev mode
// that meant a script inside the working tree kept running on every agent turn.
func syncHookCommand(groups []MatcherGroup, command string, timeoutSec int) ([]MatcherGroup, bool) {
	groups, dropped := dropStaleEntireHooks(groups, command, timeoutSec)
	if hookCommandExists(groups, command, timeoutSec) {
		return groups, dropped
	}
	return addHook(groups, command, timeoutSec), true
}

// dropStaleEntireHooks removes Entire-owned hooks that are not command at
// timeoutSec, per matcher group, pruning groups left with no hooks. See
// agent.DropStaleManagedHooks for why this runs on every install.
//
// The timeout is part of what counts as stale here, which the shared helper
// cannot express: it matches on the command alone, and Codex budgets per event.
// A SessionEnd hook left at the old 30s keeps its command but makes Codex print
// a clamping warning at every startup — see SessionEndTimeoutSec.
func dropStaleEntireHooks(groups []MatcherGroup, command string, timeoutSec int) ([]MatcherGroup, bool) {
	staleTimeout := func(e HookEntry) bool { return e.Command == command && e.Timeout != timeoutSec }

	result := make([]MatcherGroup, 0, len(groups))
	dropped := false
	for _, group := range groups {
		kept, d := agent.DropStaleManagedHooks(group.Hooks, hookEntryCommand, []string{command})
		if d {
			dropped = true
		}
		// Clone before deleting: with nothing dropped above, kept still aliases
		// the caller's slice.
		if slices.ContainsFunc(kept, staleTimeout) {
			kept = slices.DeleteFunc(slices.Clone(kept), staleTimeout)
			dropped = true
		}
		if len(kept) > 0 {
			group.Hooks = kept
			result = append(result, group)
		}
	}
	if !dropped {
		return groups, false
	}
	return result, true
}

// hookEntryCommand reads the command off a hook entry for the shared helpers.
func hookEntryCommand(e HookEntry) string { return e.Command }

func addHook(groups []MatcherGroup, command string, timeoutSec int) []MatcherGroup {
	entry := HookEntry{
		Type:    "command",
		Command: command,
		Timeout: timeoutSec,
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
	return agent.IsManagedHookCommand(command)
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
