package copilotcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// VSCodeHooksFileName is the VS Code-native hook file managed by Entire. It is
// installed alongside the Copilot CLI file (HooksFileName) so Copilot sessions
// driven from VS Code's agent hooks (Preview) are captured. See AGENT.md.
const VSCodeHooksFileName = "entire-vscode.json"

// vsCodeManagedEvents lists the VS Code events Entire registers in
// entire-vscode.json, paired with the CLI hook verb each one invokes.
//
// Only the turn hooks are registered here. VS Code reads Copilot CLI configs by
// converting lowerCamelCase event names to PascalCase (userPromptSubmitted ->
// UserPromptSubmitted, agentStop -> AgentStop). Those converted names are not
// real VS Code events, so the capture-critical turn hooks never fire from
// entire.json inside VS Code. The events whose converted names line up with a
// real VS Code event (SessionStart, SubagentStop, PreToolUse, PostToolUse) are
// already delivered by VS Code from entire.json, so registering them here too
// would only double-fire them.
//
// VS Code uses a single "Stop" event for both end-of-turn and terminal
// session-stop, where Copilot CLI distinguishes agent-stop from session-end.
// We therefore register BOTH verbs under "Stop"; validateVSCodeEvent (compat.go)
// routes each payload to the matching handler by reason, and the non-matching
// handler no-ops. Omitting session-end here would drop terminal SessionEnd
// events for VS Code-driven sessions.
var vsCodeManagedEvents = []struct {
	Event string   // VS Code hookEventName (PascalCase)
	Verbs []string // Entire CLI hook verbs registered under that event
}{
	{VSCodeEventUserPromptSubmit, []string{HookNameUserPromptSubmitted}},
	{VSCodeEventStop, []string{HookNameAgentStop, HookNameSessionEnd}},
}

// VSCodeHookEntry represents a single VS Code hook command. VS Code uses the
// "command" field rather than Copilot CLI's "bash" field, and a "timeout" field
// in seconds (not Copilot CLI's "timeoutSec"). Unknown top-level file fields and
// unknown event types are preserved on round-trip via raw maps; the optional
// per-entry fields VS Code documents are retained explicitly so user-authored
// entries survive a rewrite. See the schema at
// https://code.visualstudio.com/docs/copilot/customization/hooks.
type VSCodeHookEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Windows string            `json:"windows,omitempty"`
	Linux   string            `json:"linux,omitempty"`
	Osx     string            `json:"osx,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
	Comment string            `json:"comment,omitempty"`
}

// vsCodeHookCommand builds the command string for a VS Code hook verb. In
// production it is wrapped so a missing Entire CLI exits cleanly; the shared
// "entire hooks copilot-cli <verb>" handlers parse VS Code payload shapes.
func vsCodeHookCommand(verb string, localDev bool) string {
	if localDev {
		return agent.LocalDevHookScript + " hooks copilot-cli " + verb
	}
	return agent.WrapProductionSilentHookCommand("entire hooks copilot-cli " + verb)
}

// installVSCodeHooks writes/updates .github/hooks/entire-vscode.json with the
// VS Code turn hooks. If force is true, existing Entire entries are removed
// before installing. Unknown fields and event types are preserved on round-trip.
// Returns the number of hook entries newly added.
func (c *CopilotCLIAgent) installVSCodeHooks(worktreeRoot string, localDev bool, force bool) (int, error) {
	hooksPath := filepath.Join(worktreeRoot, hooksDir, VSCodeHooksFileName)

	rawFile, rawHooks, err := readVSCodeHooksFile(hooksPath)
	if err != nil {
		return 0, err
	}
	if rawFile == nil {
		// VS Code's hook schema has no top-level "version" field (unlike the
		// Copilot CLI's entire.json), so a fresh file is just {"hooks": {...}}.
		rawFile = make(map[string]json.RawMessage)
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	count := 0
	for _, h := range vsCodeManagedEvents {
		var entries []VSCodeHookEntry
		if err := parseVSCodeHookEvent(rawHooks, h.Event, &entries); err != nil {
			return 0, err
		}
		if force {
			entries = removeEntireVSCodeHooks(entries)
		}
		// All verbs share one event, so remove-on-force happens once above; then
		// add each missing verb. Doing the force-removal per verb would wipe a
		// sibling verb added earlier in this same loop.
		for _, verb := range h.Verbs {
			cmd := vsCodeHookCommand(verb, localDev)
			if !vsCodeCommandExists(entries, cmd) {
				entries = append(entries, VSCodeHookEntry{
					Type:    "command",
					Command: cmd,
					Comment: "Entire CLI",
				})
				count++
			}
		}
		if err := marshalVSCodeHookEvent(rawHooks, h.Event, entries); err != nil {
			return 0, err
		}
	}

	if err := writeVSCodeHooksFile(hooksPath, rawFile, rawHooks); err != nil {
		return 0, err
	}
	return count, nil
}

// uninstallVSCodeHooks removes Entire entries from entire-vscode.json. When the
// file holds nothing user-owned afterwards (no hooks of any event type and no
// top-level fields beyond the structural version/hooks keys Entire writes), it
// is deleted rather than left as an empty shell. If the user added their own
// hooks or top-level fields, those are preserved and the file is rewritten.
func (c *CopilotCLIAgent) uninstallVSCodeHooks(worktreeRoot string) error {
	hooksPath := filepath.Join(worktreeRoot, hooksDir, VSCodeHooksFileName)

	rawFile, rawHooks, err := readVSCodeHooksFile(hooksPath)
	if err != nil {
		return err
	}
	if rawFile == nil {
		return nil // No file means nothing to uninstall.
	}

	for _, h := range vsCodeManagedEvents {
		var entries []VSCodeHookEntry
		if err := parseVSCodeHookEvent(rawHooks, h.Event, &entries); err != nil {
			return err
		}
		entries = removeEntireVSCodeHooks(entries)
		if err := marshalVSCodeHookEvent(rawHooks, h.Event, entries); err != nil {
			return err
		}
	}

	// Delete the file only when nothing user-owned remains: no hooks left and no
	// top-level fields other than the structural ones Entire itself writes.
	if len(rawHooks) == 0 && !hasUserTopLevelFields(rawFile) {
		if err := os.Remove(hooksPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove %s: %w", VSCodeHooksFileName, err)
		}
		return nil
	}

	return writeVSCodeHooksFile(hooksPath, rawFile, rawHooks)
}

// hasUserTopLevelFields reports whether rawFile carries any top-level key beyond
// the structural ones — i.e. fields a user added that must survive uninstall.
// "hooks" is structural; "version" is not part of the VS Code schema but older
// Entire builds wrote it, so it is treated as non-user to keep cleanup working.
func hasUserTopLevelFields(rawFile map[string]json.RawMessage) bool {
	for key := range rawFile {
		if key != "version" && key != "hooks" {
			return true
		}
	}
	return false
}

// areVSCodeHooksInstalled reports whether any Entire hook is present in
// entire-vscode.json.
func (c *CopilotCLIAgent) areVSCodeHooksInstalled(worktreeRoot string) bool {
	hooksPath := filepath.Join(worktreeRoot, hooksDir, VSCodeHooksFileName)
	rawFile, rawHooks, err := readVSCodeHooksFile(hooksPath)
	if err != nil || rawFile == nil {
		return false
	}
	for _, h := range vsCodeManagedEvents {
		var entries []VSCodeHookEntry
		if err := parseVSCodeHookEvent(rawHooks, h.Event, &entries); err != nil {
			return false
		}
		if hasEntireVSCodeHook(entries) {
			return true
		}
	}
	return false
}

// readVSCodeHooksFile reads and parses entire-vscode.json into raw maps,
// preserving unknown fields. Returns (nil, nil, nil) when the file is absent so
// callers can distinguish "missing" from "empty".
func readVSCodeHooksFile(hooksPath string) (rawFile, rawHooks map[string]json.RawMessage, err error) {
	data, readErr := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	switch {
	case readErr == nil:
		if err := json.Unmarshal(data, &rawFile); err != nil {
			return nil, nil, fmt.Errorf("failed to parse %s: %w", VSCodeHooksFileName, err)
		}
		if hooksRaw, ok := rawFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return nil, nil, fmt.Errorf("failed to parse hooks in %s: %w", VSCodeHooksFileName, err)
			}
		}
	case errors.Is(readErr, os.ErrNotExist):
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("failed to read %s: %w", VSCodeHooksFileName, readErr)
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}
	return rawFile, rawHooks, nil
}

// writeVSCodeHooksFile marshals rawHooks back into rawFile and writes it,
// creating the hooks directory if needed. When no hooks remain the "hooks" key
// is dropped rather than written as an empty object. Callers must pass a
// non-nil rawFile.
func writeVSCodeHooksFile(hooksPath string, rawFile, rawHooks map[string]json.RawMessage) error {
	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawFile["hooks"] = hooksJSON
	} else {
		delete(rawFile, "hooks")
	}

	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", hooksDir, err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal %s: %w", VSCodeHooksFileName, err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", VSCodeHooksFileName, err)
	}
	return nil
}

// parseVSCodeHookEvent parses a specific event's entries from rawHooks.
func parseVSCodeHookEvent(rawHooks map[string]json.RawMessage, event string, target *[]VSCodeHookEntry) error {
	if data, ok := rawHooks[event]; ok {
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("invalid JSON for VS Code event %s: %w", event, err)
		}
	}
	return nil
}

// marshalVSCodeHookEvent marshals an event's entries back into rawHooks,
// removing the key when the slice is empty.
func marshalVSCodeHookEvent(rawHooks map[string]json.RawMessage, event string, entries []VSCodeHookEntry) error {
	if len(entries) == 0 {
		delete(rawHooks, event)
		return nil
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(entries)
	if err != nil {
		return fmt.Errorf("failed to marshal VS Code event %s: %w", event, err)
	}
	rawHooks[event] = data
	return nil
}

// vsCodeCommandExists reports whether an entry with the given command already exists.
func vsCodeCommandExists(entries []VSCodeHookEntry, command string) bool {
	for _, entry := range entries {
		if entry.Command == command {
			return true
		}
	}
	return false
}

// hasEntireVSCodeHook reports whether any entry is an Entire hook.
func hasEntireVSCodeHook(entries []VSCodeHookEntry) bool {
	for _, entry := range entries {
		if isEntireHook(entry.Command) {
			return true
		}
	}
	return false
}

// removeEntireVSCodeHooks returns entries with all Entire hooks removed.
func removeEntireVSCodeHooks(entries []VSCodeHookEntry) []VSCodeHookEntry {
	result := make([]VSCodeHookEntry, 0, len(entries))
	for _, entry := range entries {
		if !isEntireHook(entry.Command) {
			result = append(result, entry)
		}
	}
	return result
}
