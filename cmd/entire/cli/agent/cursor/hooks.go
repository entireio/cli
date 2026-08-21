package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure CursorAgent implements HookSupport
var (
	_ agent.HookSupport = (*CursorAgent)(nil)
)

// Cursor hook names - these become subcommands under `entire hooks cursor`
const (
	HookNameSessionStart       = "session-start"
	HookNameSessionEnd         = "session-end"
	HookNameBeforeSubmitPrompt = "before-submit-prompt"
	HookNameStop               = "stop"
	HookNamePreCompact         = "pre-compact"
	HookNameSubagentStart      = "subagent-start"
	HookNameSubagentStop       = "subagent-stop"
)

// HooksFileName is the hooks file used by Cursor.
const HooksFileName = "hooks.json"

// HookNames returns the hook verbs Cursor supports.
// These become subcommands: entire hooks cursor <verb>
func (c *CursorAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameBeforeSubmitPrompt,
		HookNameStop,
		HookNamePreCompact,
		HookNameSubagentStart,
		HookNameSubagentStop,
	}
}

// InstallHooks installs Cursor hooks in .cursor/hooks.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
// Unknown top-level fields and hook types are preserved on round-trip.
func (c *CursorAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}

	hooksPath := filepath.Join(worktreeRoot, ".cursor", HooksFileName)

	// Use raw maps to preserve unknown fields on round-trip
	var rawFile map[string]json.RawMessage
	var rawHooks map[string]json.RawMessage

	existingData, readErr := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing "+HooksFileName+": %w", err)
		}
		if hooksRaw, ok := rawFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in "+HooksFileName+": %w", err)
			}
		}
		if _, ok := rawFile["version"]; !ok {
			rawFile["version"] = json.RawMessage(`1`)
		}
	} else {
		rawFile = map[string]json.RawMessage{
			"version": json.RawMessage(`1`),
		}
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we manage
	var sessionStart, sessionEnd, beforeSubmitPrompt, stop, preCompact, subagentStart, subagentStop []CursorHookEntry
	parseCursorHookType(rawHooks, "sessionStart", &sessionStart)
	parseCursorHookType(rawHooks, "sessionEnd", &sessionEnd)
	parseCursorHookType(rawHooks, "beforeSubmitPrompt", &beforeSubmitPrompt)
	parseCursorHookType(rawHooks, "stop", &stop)
	parseCursorHookType(rawHooks, "preCompact", &preCompact)
	parseCursorHookType(rawHooks, "subagentStart", &subagentStart)
	parseCursorHookType(rawHooks, "subagentStop", &subagentStop)

	// If force is true, remove all existing Entire hooks first
	if force {
		sessionStart = removeEntireHooks(sessionStart)
		sessionEnd = removeEntireHooks(sessionEnd)
		beforeSubmitPrompt = removeEntireHooks(beforeSubmitPrompt)
		stop = removeEntireHooks(stop)
		preCompact = removeEntireHooks(preCompact)
		subagentStart = removeEntireHooks(subagentStart)
		subagentStop = removeEntireHooks(subagentStop)
	}

	// Define hook commands
	const cmdPrefix = "entire hooks cursor "

	// Cursor spawns hook commands through the native OS shell (cmd.exe on
	// Windows), so a `sh -c '…'` wrapper silently fails to launch on a
	// Windows host without a working POSIX sh — no hook fires and, because
	// this is the *silent* wrapper, no error surfaces (issue #1424).
	// UseWindowsProductionHooks probes for a runnable sh and only swaps in
	// the native cmd.exe wrapper when one is absent, so this is a no-op on
	// hosts (incl. all non-Windows) where the sh wrapper already works.
	useWindowsHooks := agent.UseWindowsProductionHooks(ctx)
	sessionStartCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+HookNameSessionStart, useWindowsHooks)
	sessionEndCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+HookNameSessionEnd, useWindowsHooks)
	beforeSubmitPromptCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+HookNameBeforeSubmitPrompt, useWindowsHooks)
	stopCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+HookNameStop, useWindowsHooks)
	preCompactCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+HookNamePreCompact, useWindowsHooks)
	subagentStartCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+HookNameSubagentStart, useWindowsHooks)
	subagentEndCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+HookNameSubagentStop, useWindowsHooks)

	count := 0

	// Sync each hook to its desired command. syncEntireHook replaces any
	// stale-form Entire hook (e.g. an sh-wrapped entry from a previous install)
	// with the current command even without --force, so a wrapper-form change —
	// notably the sh↔cmd.exe migration driven by UseWindowsProductionHooks when
	// a Windows host gains or loses a working POSIX sh — cleanly replaces rather
	// than leaving a dead duplicate entry that could double-fire (issue #1424).
	staleDropped := false
	var dropped bool
	sessionStart, count, dropped = syncEntireHook(sessionStart, sessionStartCmd, count)
	staleDropped = staleDropped || dropped
	sessionEnd, count, dropped = syncEntireHook(sessionEnd, sessionEndCmd, count)
	staleDropped = staleDropped || dropped
	beforeSubmitPrompt, count, dropped = syncEntireHook(beforeSubmitPrompt, beforeSubmitPromptCmd, count)
	staleDropped = staleDropped || dropped
	stop, count, dropped = syncEntireHook(stop, stopCmd, count)
	staleDropped = staleDropped || dropped
	preCompact, count, dropped = syncEntireHook(preCompact, preCompactCmd, count)
	staleDropped = staleDropped || dropped
	subagentStart, count, dropped = syncEntireHook(subagentStart, subagentStartCmd, count)
	staleDropped = staleDropped || dropped
	subagentStop, count, dropped = syncEntireHook(subagentStop, subagentEndCmd, count)
	staleDropped = staleDropped || dropped

	// staleDropped forces a write even when nothing was added: a config holding
	// both a stale and a current hook adds nothing, and returning early here
	// would leave the stale hook on disk.
	if count == 0 && !staleDropped {
		return 0, nil
	}

	// Marshal modified hook types back into rawHooks
	marshalCursorHookType(rawHooks, "sessionStart", sessionStart)
	marshalCursorHookType(rawHooks, "sessionEnd", sessionEnd)
	marshalCursorHookType(rawHooks, "beforeSubmitPrompt", beforeSubmitPrompt)
	marshalCursorHookType(rawHooks, "stop", stop)
	marshalCursorHookType(rawHooks, "preCompact", preCompact)
	marshalCursorHookType(rawHooks, "subagentStart", subagentStart)
	marshalCursorHookType(rawHooks, "subagentStop", subagentStop)

	// Marshal hooks and update raw file
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawFile["hooks"] = hooksJSON

	// Write to file
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .cursor directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal "+HooksFileName+": %w", err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write "+HooksFileName+": %w", err)
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from Cursor HooksFileName.
// Unknown top-level fields and hook types are preserved on round-trip.
func (c *CursorAgent) UninstallHooks(ctx context.Context) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}
	hooksPath := filepath.Join(worktreeRoot, ".cursor", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No hooks file means nothing to uninstall
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return fmt.Errorf("failed to parse "+HooksFileName+": %w", err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawFile["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks in "+HooksFileName+": %w", err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we manage
	var sessionStart, sessionEnd, beforeSubmitPrompt, stop, preCompact, subagentStart, subagentStop []CursorHookEntry
	parseCursorHookType(rawHooks, "sessionStart", &sessionStart)
	parseCursorHookType(rawHooks, "sessionEnd", &sessionEnd)
	parseCursorHookType(rawHooks, "beforeSubmitPrompt", &beforeSubmitPrompt)
	parseCursorHookType(rawHooks, "stop", &stop)
	parseCursorHookType(rawHooks, "preCompact", &preCompact)
	parseCursorHookType(rawHooks, "subagentStart", &subagentStart)
	parseCursorHookType(rawHooks, "subagentStop", &subagentStop)

	// Remove Entire hooks from all hook types
	sessionStart = removeEntireHooks(sessionStart)
	sessionEnd = removeEntireHooks(sessionEnd)
	beforeSubmitPrompt = removeEntireHooks(beforeSubmitPrompt)
	stop = removeEntireHooks(stop)
	preCompact = removeEntireHooks(preCompact)
	subagentStart = removeEntireHooks(subagentStart)
	subagentStop = removeEntireHooks(subagentStop)

	// Marshal modified hook types back into rawHooks
	marshalCursorHookType(rawHooks, "sessionStart", sessionStart)
	marshalCursorHookType(rawHooks, "sessionEnd", sessionEnd)
	marshalCursorHookType(rawHooks, "beforeSubmitPrompt", beforeSubmitPrompt)
	marshalCursorHookType(rawHooks, "stop", stop)
	marshalCursorHookType(rawHooks, "preCompact", preCompact)
	marshalCursorHookType(rawHooks, "subagentStart", subagentStart)
	marshalCursorHookType(rawHooks, "subagentStop", subagentStop)

	// Marshal hooks back (preserving unknown hook types)
	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawFile["hooks"] = hooksJSON
	} else {
		delete(rawFile, "hooks")
	}

	// Write back
	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal "+HooksFileName+": %w", err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write "+HooksFileName+": %w", err)
	}

	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (c *CursorAgent) AreHooksInstalled(ctx context.Context) bool {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}
	hooksPath := filepath.Join(worktreeRoot, ".cursor", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return false
	}

	var hooksFile CursorHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		return false
	}

	return hasEntireHook(hooksFile.Hooks.SessionStart) ||
		hasEntireHook(hooksFile.Hooks.SessionEnd) ||
		hasEntireHook(hooksFile.Hooks.BeforeSubmitPrompt) ||
		hasEntireHook(hooksFile.Hooks.Stop) ||
		hasEntireHook(hooksFile.Hooks.PreCompact) ||
		hasEntireHook(hooksFile.Hooks.SubagentStart) ||
		hasEntireHook(hooksFile.Hooks.SubagentStop)
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

// parseCursorHookType parses a specific hook type from rawHooks into the target slice.
// Silently ignores parse errors (leaves target unchanged).
func parseCursorHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]CursorHookEntry) {
	if data, ok := rawHooks[hookType]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalCursorHookType marshals a hook type back into rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalCursorHookType(rawHooks map[string]json.RawMessage, hookType string, entries []CursorHookEntry) {
	if len(entries) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(entries)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[hookType] = data
}

// Helper functions for hook management

// syncEntireHook ensures entries contains exactly the given Entire hook command
// and no other Entire-owned entry, returning the incremented count when it had to
// add one and whether it dropped a stale entry.
//
// Dropping happens even when command is already present. Checking presence first
// (as this did before) left a hook written by an older version sitting next to the
// current one, so both fired — for the removed local-dev mode that meant a script
// inside the working tree kept running on every agent turn.
func syncEntireHook(entries []CursorHookEntry, command string, count int) ([]CursorHookEntry, int, bool) {
	entries, dropped := agent.DropStaleManagedHooks(entries, hookEntryCommand, []string{command})
	if hookCommandExists(entries, command) {
		return entries, count, dropped
	}
	return append(entries, CursorHookEntry{Command: command}), count + 1, dropped
}

// hookEntryCommand reads the command off a hook entry for the shared helpers.
func hookEntryCommand(e CursorHookEntry) string { return e.Command }

func hookCommandExists(entries []CursorHookEntry, command string) bool {
	for _, entry := range entries {
		if entry.Command == command {
			return true
		}
	}
	return false
}

func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

func hasEntireHook(entries []CursorHookEntry) bool {
	for _, entry := range entries {
		if isEntireHook(entry.Command) {
			return true
		}
	}
	return false
}

func removeEntireHooks(entries []CursorHookEntry) []CursorHookEntry {
	result := make([]CursorHookEntry, 0, len(entries))
	for _, entry := range entries {
		if !isEntireHook(entry.Command) {
			result = append(result, entry)
		}
	}
	return result
}
