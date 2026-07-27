package devin

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

// HooksFileName is the standalone hooks file used by Devin CLI. Unlike
// .claude/settings.json, the hooks object is the entire file — event names
// are top-level keys with no "hooks" wrapper.
const HooksFileName = "hooks.v1.json"

// hookEventStop etc. are the Claude Code-format event names Devin recognizes
// in hooks.v1.json.
const (
	hookEventSessionStart     = "SessionStart"
	hookEventSessionEnd       = "SessionEnd"
	hookEventStop             = "Stop"
	hookEventUserPromptSubmit = "UserPromptSubmit"
	hookEventPostToolUse      = "PostToolUse"
)

// entireHookPrefixes are command prefixes that identify Entire hooks.
var entireHookPrefixes = []string{
	"entire ",
	agent.LocalDevHookScript + " ",
}

// localDevHookCommand builds a local-dev hook command for the given hook
// name, using the shared git-based launcher script (the pattern for agents
// that locate the repo root with `git rev-parse` instead of a
// ${CLAUDE_PROJECT_DIR}-style variable).
func localDevHookCommand(hookName string) string {
	return fmt.Sprintf("%s hooks devin %s", agent.LocalDevHookScript, hookName)
}

func productionHookCommand(hookName string, useWindows bool) string {
	return agent.WrapProductionSilentHookCommandForOS("entire hooks devin "+hookName, useWindows)
}

// hooksFilePath returns the absolute path of .devin/hooks.v1.json for the repo.
func hooksFilePath(ctx context.Context) (string, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}
	}
	return filepath.Join(repoRoot, ".devin", HooksFileName), nil
}

// managedHookEvents describes the hook entries Entire installs: event name,
// tool matcher, and the hook verb of the entire command to run.
var managedHookEvents = []struct {
	event   string
	matcher string
	verb    string
}{
	{hookEventSessionStart, "", HookNameSessionStart},
	{hookEventSessionEnd, "", HookNameSessionEnd},
	{hookEventStop, "", HookNameStop},
	{hookEventUserPromptSubmit, "", HookNameUserPromptSubmit},
	{hookEventPostToolUse, fileModificationToolsMatcher, HookNamePostToolUse},
}

// InstallHooks installs Devin hooks in .devin/hooks.v1.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (d *DevinAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	hooksPath, err := hooksFilePath(ctx)
	if err != nil {
		return 0, err
	}

	// The whole file is the hooks object; preserve unknown event types raw.
	rawHooks := make(map[string]json.RawMessage)
	if existingData, readErr := os.ReadFile(hooksPath); readErr == nil { //nolint:gosec // path is repo root + fixed name
		if err := json.Unmarshal(existingData, &rawHooks); err != nil {
			return 0, fmt.Errorf("failed to parse existing %s: %w", HooksFileName, err)
		}
	}

	// Devin ships a native Windows CLI; pick the cmd.exe-based wrapper when a
	// working POSIX sh is not available (shared probe, codex pattern).
	useWindows := agent.UseWindowsProductionHooks(ctx, localDev)

	count := 0
	for _, spec := range managedHookEvents {
		var matchers []HookMatcher
		parseHookEvent(rawHooks, spec.event, &matchers)

		if force {
			matchers = removeEntireHooks(matchers)
		}

		command := productionHookCommand(spec.verb, useWindows)
		if localDev {
			command = localDevHookCommand(spec.verb)
		}

		if !hookCommandExists(matchers, spec.matcher, command) {
			matchers = addHookToMatcher(matchers, spec.matcher, command)
			count++
		}
		marshalHookEvent(rawHooks, spec.event, matchers)
	}

	if count == 0 {
		return 0, nil // All hooks already installed
	}

	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .devin directory: %w", err)
	}
	output, err := jsonutil.MarshalIndentWithNewline(rawHooks, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}
	return count, nil
}

// UninstallHooks removes Entire hooks from .devin/hooks.v1.json.
func (d *DevinAgent) UninstallHooks(ctx context.Context) error {
	hooksPath, err := hooksFilePath(ctx)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is repo root + fixed name
	if err != nil {
		return nil //nolint:nilerr // No hooks file means nothing to uninstall
	}

	rawHooks := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &rawHooks); err != nil {
		return fmt.Errorf("failed to parse %s: %w", HooksFileName, err)
	}

	for _, spec := range managedHookEvents {
		var matchers []HookMatcher
		parseHookEvent(rawHooks, spec.event, &matchers)
		marshalHookEvent(rawHooks, spec.event, removeEntireHooks(matchers))
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawHooks, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks: %w", err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (d *DevinAgent) AreHooksInstalled(ctx context.Context) bool {
	hooksPath, err := hooksFilePath(ctx)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is repo root + fixed name
	if err != nil {
		return false
	}

	rawHooks := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &rawHooks); err != nil {
		return false
	}

	var stop []HookMatcher
	parseHookEvent(rawHooks, hookEventStop, &stop)
	for _, matcher := range stop {
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

// --- Helpers ---

// parseHookEvent parses a specific event's matchers from rawHooks.
// Silently ignores parse errors (leaves target unchanged).
func parseHookEvent(rawHooks map[string]json.RawMessage, event string, target *[]HookMatcher) {
	if data, ok := rawHooks[event]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalHookEvent marshals an event's matchers back to rawHooks.
// If the slice is empty, removes the key so the file stays minimal.
func marshalHookEvent(rawHooks map[string]json.RawMessage, event string, matchers []HookMatcher) {
	if len(matchers) == 0 {
		delete(rawHooks, event)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(matchers)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[event] = data
}

func hookCommandExists(matchers []HookMatcher, matcherName, command string) bool {
	for _, matcher := range matchers {
		if matcher.Matcher != matcherName {
			continue
		}
		for _, hook := range matcher.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func addHookToMatcher(matchers []HookMatcher, matcherName, command string) []HookMatcher {
	entry := HookEntry{Type: "command", Command: command}
	for i, matcher := range matchers {
		if matcher.Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}
	return append(matchers, HookMatcher{
		Matcher: matcherName,
		Hooks:   []HookEntry{entry},
	})
}

// isEntireHook checks if a command is an Entire hook (direct or wrapped form).
func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command, entireHookPrefixes)
}

// removeEntireHooks removes all Entire hooks from a list of matchers.
func removeEntireHooks(matchers []HookMatcher) []HookMatcher {
	result := make([]HookMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		filteredHooks := make([]HookEntry, 0, len(matcher.Hooks))
		for _, hook := range matcher.Hooks {
			if !isEntireHook(hook.Command) {
				filteredHooks = append(filteredHooks, hook)
			}
		}
		if len(filteredHooks) > 0 {
			matcher.Hooks = filteredHooks
			result = append(result, matcher)
		}
	}
	return result
}
