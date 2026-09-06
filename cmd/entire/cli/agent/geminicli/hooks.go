package geminicli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure GeminiCLIAgent implements HookSupport
var (
	_ agent.HookSupport       = (*GeminiCLIAgent)(nil)
	_ agent.HookConfigLocator = (*GeminiCLIAgent)(nil)
)

// Gemini CLI hook names - these become subcommands under `entire hooks gemini`
const (
	HookNameSessionStart        = "session-start"
	HookNameSessionEnd          = "session-end"
	HookNameBeforeAgent         = "before-agent"
	HookNameAfterAgent          = "after-agent"
	HookNameBeforeModel         = "before-model"
	HookNameAfterModel          = "after-model"
	HookNameBeforeToolSelection = "before-tool-selection"
	HookNameBeforeTool          = "before-tool"
	HookNameAfterTool           = "after-tool"
	HookNamePreCompress         = "pre-compress"
	HookNameNotification        = "notification"
)

// GeminiSettingsFileName is the settings file used by Gemini CLI.
const GeminiSettingsFileName = "settings.json"

// entireGeminiHookNames are the "name" fields InstallHooks writes; the name
// is Gemini's per-entry identifier and the cheapest on-disk discriminator
// between our entries and user-authored ones.
var entireGeminiHookNames = map[string]bool{
	"entire-session-start":         true,
	"entire-session-end-exit":      true,
	"entire-session-end-logout":    true,
	"entire-before-agent":          true,
	"entire-after-agent":           true,
	"entire-before-model":          true,
	"entire-after-model":           true,
	"entire-before-tool-selection": true,
	"entire-before-tool":           true,
	"entire-after-tool":            true,
	"entire-pre-compress":          true,
	"entire-notification":          true,
}

// geminiHookSpec describes one hook entry a full install writes: the settings
// section it lives in, the entry name, and the `entire hooks gemini`
// subcommand behind its command string.
type geminiHookSpec struct {
	section  string
	matcher  string
	name     string
	hookName string
	// warnWrap selects the JSON-warning production wrapper (session-start,
	// the one hook allowed to print) over the silent one.
	warnWrap bool
}

// geminiHookSpecs is the full expected entry set — the completeness spec
// behind the user-scope install and AreUserHooksInstalled. It must stay in
// lockstep with what installHooksToFile writes; the fresh-install-reads-as-
// installed regression test pins that.
var geminiHookSpecs = []geminiHookSpec{
	{"SessionStart", "", "entire-session-start", HookNameSessionStart, true},
	{"SessionEnd", "exit", "entire-session-end-exit", HookNameSessionEnd, false},
	{"SessionEnd", "logout", "entire-session-end-logout", HookNameSessionEnd, false},
	{"BeforeAgent", "", "entire-before-agent", HookNameBeforeAgent, false},
	{"AfterAgent", "", "entire-after-agent", HookNameAfterAgent, false},
	{"BeforeModel", "", "entire-before-model", HookNameBeforeModel, false},
	{"AfterModel", "", "entire-after-model", HookNameAfterModel, false},
	{"BeforeToolSelection", "", "entire-before-tool-selection", HookNameBeforeToolSelection, false},
	{"BeforeTool", "*", "entire-before-tool", HookNameBeforeTool, false},
	{"AfterTool", "*", "entire-after-tool", HookNameAfterTool, false},
	{"PreCompress", "", "entire-pre-compress", HookNamePreCompress, false},
	{"Notification", "", "entire-notification", HookNameNotification, false},
}

// productionCommand returns the exact production command an install writes
// for this spec.
func (s geminiHookSpec) productionCommand() string {
	cmd := "entire hooks gemini " + s.hookName
	if s.warnWrap {
		return agent.WrapProductionJSONWarningHookCommand(cmd, agent.WarningFormatSingleLine)
	}
	return agent.WrapProductionSilentHookCommand(cmd)
}

// userHooksCurrent is the user-scope completeness predicate: every expected
// entry present, in current production form, in its section. "Any Entire hook
// present" is not enough there — a file with SessionStart intact but the other
// entries deleted would read as covered (status, doctor) and short-circuit the
// install while most hooks never fire. Entries are matched by name+command
// within the section, tolerating a user-adjusted matcher, mirroring the Claude
// Code predicate's leniency.
func userHooksCurrent(sections map[string]*[]GeminiHookMatcher) bool {
	for _, spec := range geminiHookSpecs {
		matchers := sections[spec.section]
		if matchers == nil || !sectionHasEntry(*matchers, spec.name, spec.productionCommand()) {
			return false
		}
	}
	return true
}

func sectionHasEntry(matchers []GeminiHookMatcher, name, command string) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if hook.Name == name && hook.Command == command {
				return true
			}
		}
	}
	return false
}

// hookSections exposes the managed hook sections keyed by their settings-file
// key — the same shape installHooksToFile parses — so the completeness
// predicate can run against either representation.
func (h *GeminiHooks) hookSections() map[string]*[]GeminiHookMatcher {
	return map[string]*[]GeminiHookMatcher{
		"SessionStart":        &h.SessionStart,
		"SessionEnd":          &h.SessionEnd,
		"BeforeAgent":         &h.BeforeAgent,
		"AfterAgent":          &h.AfterAgent,
		"BeforeModel":         &h.BeforeModel,
		"AfterModel":          &h.AfterModel,
		"BeforeToolSelection": &h.BeforeToolSelection,
		"BeforeTool":          &h.BeforeTool,
		"AfterTool":           &h.AfterTool,
		"PreCompress":         &h.PreCompress,
		"Notification":        &h.Notification,
	}
}

func newGeminiHookSections() map[string]*[]GeminiHookMatcher {
	return (&GeminiHooks{}).hookSections()
}

// areUserHooksCurrentInFile reports whether the settings file carries the FULL
// expected Entire entry set in current production form — the user-scope
// completeness predicate the user-scope install repairs to. A missing file is
// an fs.ErrNotExist error, matching areHooksInstalledInFile.
func areUserHooksCurrentInFile(file hookSettingsIO) (bool, error) {
	data, err := file.Read()
	if err != nil {
		return false, fmt.Errorf("read %s: %w", file.Path(), err)
	}
	var settings GeminiSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, fmt.Errorf("parse %s: %w", file.Path(), err)
	}
	return userHooksCurrent(settings.Hooks.hookSections()), nil
}

// hookSettingsIO abstracts where a hook install reads and writes its settings
// file. Repo scope uses agent.HookConfigFile: the path lives in the working
// tree, which arrives by clone, so a checked-in symlink at .gemini must be
// refused rather than followed. User scope (~/.gemini) is the opposite case —
// dotfile managers legitimately symlink it — so it follows symlinks
// (jsonutil.WriteFileAtomicFollowingSymlinks) under the user-hook lock.
type hookSettingsIO interface {
	Read() ([]byte, error)
	Write(data []byte, perm os.FileMode) error
	Path() string
}

// userSettingsIO is the user-scope implementation of hookSettingsIO.
type userSettingsIO struct{ path string }

func (u userSettingsIO) Path() string { return u.path }

func (u userSettingsIO) Read() ([]byte, error) {
	return os.ReadFile(u.path) //nolint:wrapcheck // fixed user-level settings location; callers name the file
}

func (u userSettingsIO) Write(data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(u.path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(u.path), err)
	}
	return jsonutil.WriteFileAtomicFollowingSymlinks(u.path, data, perm) //nolint:wrapcheck // callers name the file
}

// geminiHookConfig returns .gemini/settings.json for the current worktree,
// opened through the worktree's root. Every repo-scope read and write of that
// file goes through it: the directory lives in the working tree, which arrives
// by clone, so a checked-in symlink at `.gemini` must not be something Entire
// creates directories under and writes through.
func geminiHookConfig(ctx context.Context) (*agent.HookConfigFile, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return nil, fmt.Errorf("failed to get current directory: %w", err)
		}
	}
	return agent.OpenHookConfig(repoRoot, (&GeminiCLIAgent{}).HookConfigRelPath()) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// InstallHooks installs Gemini CLI hooks in .gemini/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (g *GeminiCLIAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	cfg, err := geminiHookConfig(ctx)
	if err != nil {
		return 0, err
	}
	count, _, err := installHooksToFile(ctx, cfg, force, false)
	return count, err
}

// installHooksToFile installs Entire's Gemini CLI hooks into the settings
// file at settingsPath, preserving every unrelated key. Shared by the
// repo-level install above and the user-level install (InstallUserHooks).
//
// userScope switches the idempotency check from "the SessionStart entry is in
// current form" to the full completeness predicate (userHooksCurrent): a
// user-level file with SessionStart intact but other Entire entries deleted
// must be repaired (strip ours, re-add the full set), not short-circuited.
// repaired reports a user-scope rewrite that touched pre-existing Entire
// entries or legacy fields, so the caller can report the repair instead of
// "already installed". Repo-scope behavior is unchanged.
func installHooksToFile(ctx context.Context, file hookSettingsIO, force, userScope bool) (count int, repaired bool, err error) {
	settingsPath := file.Path()
	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage

	// hooksConfig is held raw so every key except "enabled" (the one Entire
	// manages) round-trips: decoding into a typed struct dropped user keys
	// like "timeout" on write-back.
	var hooksConfig map[string]json.RawMessage

	existingData, readErr := file.Read()
	switch {
	case readErr == nil:
		if err := json.Unmarshal(existingData, &rawSettings); err != nil {
			return 0, false, fmt.Errorf("failed to parse existing %s: %w", settingsPath, err)
		}
		if hooksRaw, ok := rawSettings["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, false, fmt.Errorf("failed to parse hooks in %s: %w", settingsPath, err)
			}
		}
		if hooksConfigRaw, ok := rawSettings["hooksConfig"]; ok {
			if err := json.Unmarshal(hooksConfigRaw, &hooksConfig); err != nil {
				return 0, false, fmt.Errorf("failed to parse hooksConfig in %s: %w", settingsPath, err)
			}
		}
	case errors.Is(readErr, fs.ErrNotExist):
		rawSettings = make(map[string]json.RawMessage)
	default:
		// Only a genuinely missing file means "start fresh". Any other read
		// failure (permissions, I/O) must abort: proceeding would replace the
		// user's whole settings file with an Entire-only one.
		return 0, false, fmt.Errorf("failed to read %s: %w", settingsPath, readErr)
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}
	if hooksConfig == nil {
		hooksConfig = make(map[string]json.RawMessage)
	}

	cleanupDone := stripLegacyHooksEnabledField(ctx, rawHooks)

	// hooksConfig.enabled must be true for Gemini CLI to execute hooks.
	hooksConfig["enabled"] = json.RawMessage("true")

	const cmdPrefix = "entire hooks gemini "
	wantCommands := make([]string, 0, len(geminiHookSpecs))
	for _, spec := range geminiHookSpecs {
		wantCommands = append(wantCommands, spec.productionCommand())
	}
	sections := newGeminiHookSections()
	if err := parseGeminiHookSections(rawHooks, settingsPath, sections); err != nil {
		return 0, false, err
	}

	// Check for idempotency BEFORE removing hooks. When cleanupDone, we still
	// need to write the file to persist the cleanup, but we return 0 (not 12)
	// so callers know no hooks were added.
	if !force && installAlreadyCurrent(sections, cmdPrefix, userScope) &&
		!hasStaleEntireHook(sectionLists(sections), wantCommands) {
		if !cleanupDone {
			return 0, false, nil // Already installed with same mode, nothing to write
		}
		// The legacy-field cleanup rewrote an otherwise-current file: a
		// repair, not "already installed".
		return 0, userScope, writeGeminiSettingsFile(rawSettings, rawHooks, hooksConfig, file)
	}

	// A pre-existing Entire entry in any section means the rewrite below is a
	// repair (partial, duplicate, legacy, or alternate-form install
	// normalized), not a fresh install.
	hadOurs := anyEntireHook(sections)

	for _, matchers := range sections {
		*matchers = removeEntireHooks(*matchers)
	}
	for _, spec := range geminiHookSpecs {
		matchers := sections[spec.section]
		*matchers = addGeminiHook(*matchers, spec.matcher, spec.name, spec.productionCommand())
	}
	count = len(geminiHookSpecs)
	for section, matchers := range sections {
		marshalGeminiHookType(rawHooks, section, *matchers)
	}

	if err := writeGeminiSettingsFile(rawSettings, rawHooks, hooksConfig, file); err != nil {
		return 0, false, err
	}
	return count, userScope && hadOurs, nil
}

// installAlreadyCurrent is installHooksToFile's idempotency check. User scope
// requires the FULL entry set in current form (a partial file falls through to
// the repair pass); repo scope keeps the historical check, where the
// SessionStart entry in the exact current-mode form stands in for the whole
// install.
func installAlreadyCurrent(sections map[string]*[]GeminiHookMatcher, cmdPrefix string, userScope bool) bool {
	// An exact duplicate of a current entry is not "current": it fires the
	// hook twice per event — machine-wide in user scope — and only the
	// remove-and-re-add pass below heals it (Claude Code's user-hook
	// installer forces that pass for the same reason).
	if hasDuplicateEntireHook(sections) {
		return false
	}
	if userScope {
		return userHooksCurrent(sections)
	}
	expectedCmd := agent.WrapProductionJSONWarningHookCommand(cmdPrefix+"session-start", agent.WarningFormatSingleLine)
	return getFirstEntireHookCommand(*sections["SessionStart"]) == expectedCmd
}

// hasDuplicateEntireHook reports whether any managed section carries the same
// Entire command more than once under the same matcher. The matcher is part
// of the key: SessionEnd legitimately lists one session-end command under
// both the "exit" and "logout" matchers.
func hasDuplicateEntireHook(sections map[string]*[]GeminiHookMatcher) bool {
	type entryKey struct{ matcher, command string }
	for _, matchers := range sections {
		seen := make(map[entryKey]struct{})
		for _, matcher := range *matchers {
			for _, hook := range matcher.Hooks {
				if !isEntireHookEntry(hook) {
					continue
				}
				key := entryKey{matcher.Matcher, hook.Command}
				if _, dup := seen[key]; dup {
					return true
				}
				seen[key] = struct{}{}
			}
		}
	}
	return false
}

// anyEntireHook reports whether any managed section carries an entry
// recognized as Entire's.
func anyEntireHook(sections map[string]*[]GeminiHookMatcher) bool {
	for _, matchers := range sections {
		if hasEntireHook(*matchers) {
			return true
		}
	}
	return false
}

// stripLegacyHooksEnabledField removes the legacy "enabled" boolean that old
// Entire versions wrote directly into hooks, which Gemini CLI 0.33+ rejects
// because hooks.additionalProperties requires arrays. Only that one known key
// is touched: every other non-array member of hooks is user data and must
// round-trip. Returns true if the field was removed.
func stripLegacyHooksEnabledField(ctx context.Context, rawHooks map[string]json.RawMessage) bool {
	val, ok := rawHooks["enabled"]
	if !ok {
		return false
	}
	trimmed := bytes.TrimSpace(val)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return false // an array under this key is a (strange) hook list, not the legacy field
	}
	delete(rawHooks, "enabled")
	logging.Debug(ctx, "removed legacy non-array field from hooks", slog.String("key", "enabled"))
	return true
}

// writeGeminiSettingsFile marshals rawHooks and hooksConfig back into rawSettings and writes to disk.
func writeGeminiSettingsFile(rawSettings map[string]json.RawMessage, rawHooks map[string]json.RawMessage, hooksConfig map[string]json.RawMessage, file hookSettingsIO) error {
	hooksConfigJSON, err := jsonutil.MarshalWithNoHTMLEscape(hooksConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal hooksConfig: %w", err)
	}
	rawSettings["hooksConfig"] = hooksConfigJSON

	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	// Repo scope creates .gemini with MkdirAllNoSymlink inside the write; user
	// scope follows dotfile-manager symlinks instead (see hookSettingsIO).
	if err := file.Write(output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", file.Path(), err)
	}
	return nil
}

// parseGeminiHookSections parses the hook types Entire manages out of
// rawHooks. A section that exists but does not parse as []GeminiHookMatcher
// aborts with an error naming the section and file: these sections get
// rewritten on the way out, so an unparseable one cannot round-trip verbatim
// — silently treating it as empty would clobber it on install and delete it
// on uninstall.
func parseGeminiHookSections(rawHooks map[string]json.RawMessage, settingsPath string, sections map[string]*[]GeminiHookMatcher) error {
	for hookType, target := range sections {
		data, ok := rawHooks[hookType]
		if !ok {
			continue
		}
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("hooks.%s in %s has an unexpected shape (fix or remove that section): %w", hookType, settingsPath, err)
		}
	}
	return nil
}

// marshalGeminiHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalGeminiHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []GeminiHookMatcher) {
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

// UninstallHooks removes Entire hooks from Gemini CLI settings.
func (g *GeminiCLIAgent) UninstallHooks(ctx context.Context) error {
	cfg, err := geminiHookConfig(ctx)
	if err != nil {
		return err
	}
	return uninstallHooksFromFile(ctx, cfg)
}

// uninstallHooksFromFile removes Entire hooks (and only Entire hooks) from
// the settings file at settingsPath, preserving every unrelated key.
func uninstallHooksFromFile(ctx context.Context, file hookSettingsIO) error {
	settingsPath := file.Path()
	data, err := file.Read()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // No settings file means nothing to uninstall
		}
		return fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse %s: %w", settingsPath, err)
	}

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks in %s: %w", settingsPath, err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Strip the legacy hooks.enabled field (same migration as InstallHooks)
	stripLegacyHooksEnabledField(ctx, rawHooks)

	sections := newGeminiHookSections()
	if err := parseGeminiHookSections(rawHooks, settingsPath, sections); err != nil {
		return err
	}
	for section, matchers := range sections {
		*matchers = removeEntireHooks(*matchers)
		marshalGeminiHookType(rawHooks, section, *matchers)
	}

	// Marshal hooks back (preserving unknown hook types)
	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawSettings["hooks"] = hooksJSON
	} else {
		delete(rawSettings, "hooks")
		// Install set hooksConfig.enabled so Gemini would run our hooks. With
		// no hooks left it only serves that purpose, so uninstall takes it
		// back too; while any user hook remains it is left alone, since
		// removing it would silently switch theirs off.
		if err := dropHooksConfigEnabled(rawSettings, settingsPath); err != nil {
			return err
		}
	}

	// Write back
	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := file.Write(output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", settingsPath, err)
	}
	return nil
}

// dropHooksConfigEnabled removes hooksConfig.enabled, and hooksConfig itself
// once empty, leaving every other hooksConfig key untouched.
func dropHooksConfigEnabled(rawSettings map[string]json.RawMessage, settingsPath string) error {
	hooksConfigRaw, ok := rawSettings["hooksConfig"]
	if !ok {
		return nil
	}
	var hooksConfig map[string]json.RawMessage
	if err := json.Unmarshal(hooksConfigRaw, &hooksConfig); err != nil {
		return fmt.Errorf("failed to parse hooksConfig in %s: %w", settingsPath, err)
	}
	delete(hooksConfig, "enabled")
	if len(hooksConfig) == 0 {
		delete(rawSettings, "hooksConfig")
		return nil
	}
	hooksConfigJSON, err := jsonutil.MarshalWithNoHTMLEscape(hooksConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal hooksConfig: %w", err)
	}
	rawSettings["hooksConfig"] = hooksConfigJSON
	return nil
}

// AreHooksInstalled reports whether Entire hooks are installed; a missing
// settings file is a clean "no", while an unreadable or malformed one is an
// error — "not installed" and "cannot tell" must not collapse.
func (g *GeminiCLIAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	cfg, err := geminiHookConfig(ctx)
	if err != nil {
		return false, err
	}
	installed, err := areHooksInstalledInFile(cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		logging.Warn(ctx, "gemini: failed to read settings file", "path", cfg.Path(), "err", err)
		return false, err
	}
	return installed, nil
}

// areHooksInstalledInFile reports whether any Entire hook is present in the
// settings file at settingsPath. A missing file is an fs.ErrNotExist error;
// callers that need to distinguish "not installed" from "cannot tell" branch
// on errors.Is.
func areHooksInstalledInFile(file hookSettingsIO) (bool, error) {
	data, err := file.Read()
	if err != nil {
		return false, fmt.Errorf("read %s: %w", file.Path(), err)
	}

	var settings GeminiSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, fmt.Errorf("parse %s: %w", file.Path(), err)
	}

	// Check for at least one of our hooks using isEntireHook (matches legacy hook shapes too)
	return hasEntireHook(settings.Hooks.SessionStart) ||
		hasEntireHook(settings.Hooks.SessionEnd) ||
		hasEntireHook(settings.Hooks.BeforeAgent) ||
		hasEntireHook(settings.Hooks.AfterAgent) ||
		hasEntireHook(settings.Hooks.BeforeModel) ||
		hasEntireHook(settings.Hooks.AfterModel) ||
		hasEntireHook(settings.Hooks.BeforeToolSelection) ||
		hasEntireHook(settings.Hooks.BeforeTool) ||
		hasEntireHook(settings.Hooks.AfterTool) ||
		hasEntireHook(settings.Hooks.PreCompress) ||
		hasEntireHook(settings.Hooks.Notification), nil
}

// Helper functions for hook management

// addGeminiHook adds a hook entry to matchers.
// Unlike Claude Code, Gemini hooks require a "name" field.
func addGeminiHook(matchers []GeminiHookMatcher, matcherName, hookName, command string) []GeminiHookMatcher {
	entry := GeminiHookEntry{
		Name:    hookName,
		Type:    "command",
		Command: command,
	}

	// Find or create matcher
	for i, matcher := range matchers {
		if matcher.Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}

	// Create new matcher
	newMatcher := GeminiHookMatcher{
		Hooks: []GeminiHookEntry{entry},
	}
	if matcherName != "" {
		newMatcher.Matcher = matcherName
	}
	return append(matchers, newMatcher)
}

// isEntireHook checks if a command is an Entire hook
func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

// isEntireHookEntry reports whether a hook entry is Entire's: by its
// command's managed prefix (which covers every current and legacy form an
// install ever wrote, including entries whose name was hand-edited), or — for
// an entry with no command at all — by the reserved "name" InstallHooks
// writes. A reserved name over a foreign command is NOT claimed: that is a
// user-authored hook built from an Entire entry as a template, and claiming
// it by name silently deleted it on install and uninstall.
func isEntireHookEntry(hook GeminiHookEntry) bool {
	if isEntireHook(hook.Command) {
		return true
	}
	return hook.Command == "" && entireGeminiHookNames[hook.Name]
}

// hasEntireHook checks if any hook in the matchers is an Entire hook
func hasEntireHook(matchers []GeminiHookMatcher) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHookEntry(hook) {
				return true
			}
		}
	}
	return false
}

// getFirstEntireHookCommand returns the command of the first Entire hook found, or empty string
func getFirstEntireHookCommand(matchers []GeminiHookMatcher) string {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHookEntry(hook) {
				return hook.Command
			}
		}
	}
	return ""
}

// sectionLists returns every managed section's matcher list, for scans that
// must look beyond the section installAlreadyCurrent samples.
func sectionLists(sections map[string]*[]GeminiHookMatcher) [][]GeminiHookMatcher {
	lists := make([][]GeminiHookMatcher, 0, len(sections))
	for _, l := range sections {
		lists = append(lists, *l)
	}
	return lists
}

// hasStaleEntireHook reports whether any list holds an Entire-owned hook whose
// command is not in want — i.e. a hook this version would not write. Foreign
// hooks are ignored; only commands recognized as Entire-managed count, which
// includes the shapes older versions wrote (the go-run prefixes above).
func hasStaleEntireHook(lists [][]GeminiHookMatcher, want []string) bool {
	for _, list := range lists {
		for _, matcher := range list {
			for _, hook := range matcher.Hooks {
				if isEntireHookEntry(hook) && !slices.Contains(want, hook.Command) {
					return true
				}
			}
		}
	}
	return false
}

// removeEntireHooks removes all Entire hooks from a list of matchers.
func removeEntireHooks(matchers []GeminiHookMatcher) []GeminiHookMatcher {
	result := make([]GeminiHookMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		filteredHooks := make([]GeminiHookEntry, 0, len(matcher.Hooks))
		for _, hook := range matcher.Hooks {
			if !isEntireHookEntry(hook) {
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

// HookConfigRelPath implements agent.HookConfigLocator.
func (g *GeminiCLIAgent) HookConfigRelPath() string { return ".gemini/" + GeminiSettingsFileName }
