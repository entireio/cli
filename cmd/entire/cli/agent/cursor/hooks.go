package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure CursorAgent implements HookSupport
var (
	_ agent.HookSupport       = (*CursorAgent)(nil)
	_ agent.HookConfigLocator = (*CursorAgent)(nil)
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

// cursorHookConfig returns .cursor/hooks.json for the current worktree, opened through the
// worktree's root. That directory lives in the working tree, which arrives by
// clone, so a checked-in symlink at `.cursor` must not be something Entire creates
// directories under and writes through. See agent.HookConfigFile.
func cursorHookConfig(ctx context.Context) (*agent.HookConfigFile, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Not a repository (tests, and `enable` before `git init`): the process
		// directory is the only candidate, and it is a directory the caller
		// chose rather than one derived from anything read off disk.
		worktreeRoot = "."
	}
	return agent.OpenHookConfig(worktreeRoot, (&CursorAgent{}).HookConfigRelPath()) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// hookCommandPrefix is what every hook command Entire writes for Cursor begins
// with; the verb is one of the HookName* constants.
const hookCommandPrefix = "entire hooks cursor "

// silentHookCommand wraps one hook verb in the silent production wrapper for
// this host.
//
// Cursor runs every hook command through **PowerShell** on Windows — not
// cmd.exe. Read out of the shipped builds (Cursor IDE 3.19.19 win32/x64, and
// the native cursor-agent CLI windows/x64 2026.09.08-6caf4ff), the spawn is
// `<pwsh|powershell> [-NoProfile -NonInteractive -ExecutionPolicy Bypass] -c
// "$OutputEncoding = [System.Text.Encoding]::UTF8; Get-Content -LiteralPath
// '<tmp>\cursor-hook-payload-*.json' -Raw | & { $input | <command> }"`, with the
// stored command inserted verbatim. Both Windows runners read the same
// .cursor/hooks.json, and both compose that identically.
//
// So the sh wrapper is not mangled the way droid's is: PowerShell single quotes
// are literal, and the whole script reaches sh as one argument. It fails for a
// different reason — `sh` has to resolve in the PATH of the PowerShell child
// Cursor spawns, and agent.UseWindowsProductionHooks probes for sh in the
// `entire enable` process instead. Git for Windows installs sh.exe under
// …\Git\usr\bin and …\Git\bin, neither of which is on the machine PATH (only
// …\Git\cmd is), while MSYS translates PATH for native children — so running
// `entire enable` from Git Bash passes the probe and writes a wrapper Cursor
// cannot run. That is issue #1424's reported environment (Windows 11 + Git
// Bash), and no stronger probe COMMAND fixes it, because the probe is measuring
// the wrong process. Hence agent.HookHostIsWindows rather than the probe.
//
// The failure is invisible rather than swallowed: a CommandNotFoundException
// raised inside `& { $input | … }` does not set powershell.exe's exit code, so
// Cursor sees exit 0 and has no error to report. The same command standalone
// exits 1. TestWindowsWrappers_CursorComposition asserts this on a real Windows
// runner — the sh-wrapper subtest is exactly this case.
//
// This moves the PATH dependency rather than removing it — the wrapper opens
// with `where.exe entire`, so it needs `entire` on the child's PATH, which every
// documented install method satisfies and Git for Windows' sh does not.
//
// Not fixed by any wrapper: the cursor-agent CLI picks a bash shell executor
// whenever MSYSTEM is set (i.e. started from Git Bash) while still composing the
// PowerShell script above, so hooks cannot run there at all. That is Cursor's to
// fix, and it does not affect the IDE.
func silentHookCommand(verb string, useWindows bool) string {
	return agent.WrapProductionSilentHookCommandForOS(hookCommandPrefix+verb, useWindows)
}

// InstallHooks installs Cursor hooks in .cursor/hooks.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
// Unknown top-level fields and hook types are preserved on round-trip.
func (c *CursorAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	cfg, err := cursorHookConfig(ctx)
	if err != nil {
		return 0, err
	}

	// Use raw maps to preserve unknown fields on round-trip
	var rawFile map[string]json.RawMessage
	var rawHooks map[string]json.RawMessage

	existingData, readErr := cfg.Read()
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

	// Define hook commands. See silentHookCommand for why the host alone
	// decides the wrapper.
	useWindowsHooks := agent.HookHostIsWindows()
	sessionStartCmd := silentHookCommand(HookNameSessionStart, useWindowsHooks)
	sessionEndCmd := silentHookCommand(HookNameSessionEnd, useWindowsHooks)
	beforeSubmitPromptCmd := silentHookCommand(HookNameBeforeSubmitPrompt, useWindowsHooks)
	stopCmd := silentHookCommand(HookNameStop, useWindowsHooks)
	preCompactCmd := silentHookCommand(HookNamePreCompact, useWindowsHooks)
	subagentStartCmd := silentHookCommand(HookNameSubagentStart, useWindowsHooks)
	subagentEndCmd := silentHookCommand(HookNameSubagentStop, useWindowsHooks)

	count := 0

	// Sync each hook to its desired command. syncEntireHook replaces any
	// stale-form Entire hook (e.g. an sh-wrapped entry from a previous install)
	// with the current command even without --force, so the sh→cmd.exe migration
	// on an already-enabled Windows repo cleanly replaces rather than leaving a
	// dead duplicate entry that could double-fire (issue #1424).
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
	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal "+HooksFileName+": %w", err)
	}

	if err := cfg.Write(output, 0o600); err != nil {
		return 0, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from Cursor HooksFileName.
// Unknown top-level fields and hook types are preserved on round-trip.
func (c *CursorAgent) UninstallHooks(ctx context.Context) error {
	cfg, err := cursorHookConfig(ctx)
	if err != nil {
		return err
	}
	data, err := cfg.Read()
	if err != nil {
		// An absent file means nothing to uninstall; an unreadable one does not.
		// Collapsing both leaves hooks on disk while reporting success.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", cfg.Path(), err)
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

	if err := cfg.Write(output, 0o600); err != nil {
		return err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}

	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
//
// A missing config file is an answer, not a failure: that file is where the
// state lives, so its absence means no hooks. Anything that stops us reading the
// answer — an unreadable file, malformed config — is returned as an error, since
// "we could not tell" and "there are none" are different things to a caller
// deciding whether hooks can be left alone.
func (c *CursorAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	cfg, err := cursorHookConfig(ctx)
	if err != nil {
		return false, err
	}
	data, err := cfg.Read()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		logging.Warn(ctx, "cursor: failed to read hooks file", "path", cfg.Path(), "err", err)
		return false, fmt.Errorf("read %s: %w", cfg.Path(), err)
	}

	var hooksFile CursorHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		logging.Warn(ctx, "cursor: failed to parse hooks file", "path", cfg.Path(), "err", err)
		return false, fmt.Errorf("parse hook config: %w", err)
	}

	return hasEntireHook(hooksFile.Hooks.SessionStart) ||
		hasEntireHook(hooksFile.Hooks.SessionEnd) ||
		hasEntireHook(hooksFile.Hooks.BeforeSubmitPrompt) ||
		hasEntireHook(hooksFile.Hooks.Stop) ||
		hasEntireHook(hooksFile.Hooks.PreCompact) ||
		hasEntireHook(hooksFile.Hooks.SubagentStart) ||
		hasEntireHook(hooksFile.Hooks.SubagentStop), nil
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

// HookConfigRelPath implements agent.HookConfigLocator.
func (c *CursorAgent) HookConfigRelPath() string { return ".cursor/" + HooksFileName }
