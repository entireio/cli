package kiro

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

// Ensure KiroAgent implements HookSupport
var _ agent.HookSupport = (*KiroAgent)(nil)

// Kiro hook names - these become subcommands under `entire hooks kiro`
const (
	HookNameAgentSpawn       = "agent-spawn"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNameStop             = "stop"
	HookNamePreToolUse       = "pre-tool-use"
	HookNamePostToolUse      = "post-tool-use"
)

// HooksFileName is the hooks file used by Kiro.
const HooksFileName = "hooks.json"

// entireHookPrefixes are command prefixes that identify Entire hooks
var entireHookPrefixes = []string{
	"entire ",
	"go run ${KIRO_PROJECT_DIR}/cmd/entire/main.go ",
}

// HookNames returns the hook verbs Kiro supports.
// These become subcommands: entire hooks kiro <verb>
func (k *KiroAgent) HookNames() []string {
	return []string{
		HookNameAgentSpawn,
		HookNameUserPromptSubmit,
		HookNameStop,
		HookNamePreToolUse,
		HookNamePostToolUse,
	}
}

// InstallHooks installs Kiro hooks in .kiro/hooks.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
// Unknown top-level fields and hook types are preserved on round-trip.
func (k *KiroAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}

	hooksPath := filepath.Join(worktreeRoot, ".kiro", HooksFileName)

	// Use raw maps to preserve unknown fields on round-trip
	var rawFile map[string]json.RawMessage
	var rawHooks map[string]json.RawMessage

	existingData, readErr := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing %s: %w", HooksFileName, err)
		}
		if hooksRaw, ok := rawFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in %s: %w", HooksFileName, err)
			}
		}
	} else {
		rawFile = make(map[string]json.RawMessage)
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we manage
	var agentSpawn, userPromptSubmit, stop, preToolUse, postToolUse []KiroHookEntry
	parseKiroHookType(rawHooks, "agentSpawn", &agentSpawn)
	parseKiroHookType(rawHooks, "userPromptSubmit", &userPromptSubmit)
	parseKiroHookType(rawHooks, "stop", &stop)
	parseKiroHookType(rawHooks, "preToolUse", &preToolUse)
	parseKiroHookType(rawHooks, "postToolUse", &postToolUse)

	// If force is true, remove all existing Entire hooks first
	if force {
		agentSpawn = removeEntireHooks(agentSpawn)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		stop = removeEntireHooks(stop)
		preToolUse = removeEntireHooks(preToolUse)
		postToolUse = removeEntireHooks(postToolUse)
	}

	// Define hook commands
	var cmdPrefix string
	if localDev {
		cmdPrefix = "go run ${KIRO_PROJECT_DIR}/cmd/entire/main.go hooks kiro "
	} else {
		cmdPrefix = "entire hooks kiro "
	}

	agentSpawnCmd := cmdPrefix + HookNameAgentSpawn
	userPromptSubmitCmd := cmdPrefix + HookNameUserPromptSubmit
	stopCmd := cmdPrefix + HookNameStop
	preToolUseCmd := cmdPrefix + HookNamePreToolUse
	postToolUseCmd := cmdPrefix + HookNamePostToolUse

	count := 0

	// Add hooks if they don't exist
	if !hookCommandExists(agentSpawn, agentSpawnCmd) {
		agentSpawn = append(agentSpawn, KiroHookEntry{Command: agentSpawnCmd})
		count++
	}
	if !hookCommandExists(userPromptSubmit, userPromptSubmitCmd) {
		userPromptSubmit = append(userPromptSubmit, KiroHookEntry{Command: userPromptSubmitCmd})
		count++
	}
	if !hookCommandExists(stop, stopCmd) {
		stop = append(stop, KiroHookEntry{Command: stopCmd})
		count++
	}
	if !hookCommandExists(preToolUse, preToolUseCmd) {
		preToolUse = append(preToolUse, KiroHookEntry{Command: preToolUseCmd})
		count++
	}
	if !hookCommandExists(postToolUse, postToolUseCmd) {
		postToolUse = append(postToolUse, KiroHookEntry{Command: postToolUseCmd})
		count++
	}

	if count == 0 {
		return 0, nil
	}

	// Marshal modified hook types back into rawHooks
	marshalKiroHookType(rawHooks, "agentSpawn", agentSpawn)
	marshalKiroHookType(rawHooks, "userPromptSubmit", userPromptSubmit)
	marshalKiroHookType(rawHooks, "stop", stop)
	marshalKiroHookType(rawHooks, "preToolUse", preToolUse)
	marshalKiroHookType(rawHooks, "postToolUse", postToolUse)

	// Marshal hooks and update raw file
	hooksJSON, err := json.Marshal(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawFile["hooks"] = hooksJSON

	// Write to file
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .kiro directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal %s: %w", HooksFileName, err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from Kiro hooks.json.
// Unknown top-level fields and hook types are preserved on round-trip.
func (k *KiroAgent) UninstallHooks(ctx context.Context) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}
	hooksPath := filepath.Join(worktreeRoot, ".kiro", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No hooks file means nothing to uninstall
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return fmt.Errorf("failed to parse %s: %w", HooksFileName, err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawFile["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks in %s: %w", HooksFileName, err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we manage
	var agentSpawn, userPromptSubmit, stop, preToolUse, postToolUse []KiroHookEntry
	parseKiroHookType(rawHooks, "agentSpawn", &agentSpawn)
	parseKiroHookType(rawHooks, "userPromptSubmit", &userPromptSubmit)
	parseKiroHookType(rawHooks, "stop", &stop)
	parseKiroHookType(rawHooks, "preToolUse", &preToolUse)
	parseKiroHookType(rawHooks, "postToolUse", &postToolUse)

	// Remove Entire hooks from all hook types
	agentSpawn = removeEntireHooks(agentSpawn)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	stop = removeEntireHooks(stop)
	preToolUse = removeEntireHooks(preToolUse)
	postToolUse = removeEntireHooks(postToolUse)

	// Marshal modified hook types back into rawHooks
	marshalKiroHookType(rawHooks, "agentSpawn", agentSpawn)
	marshalKiroHookType(rawHooks, "userPromptSubmit", userPromptSubmit)
	marshalKiroHookType(rawHooks, "stop", stop)
	marshalKiroHookType(rawHooks, "preToolUse", preToolUse)
	marshalKiroHookType(rawHooks, "postToolUse", postToolUse)

	// Marshal hooks back (preserving unknown hook types)
	if len(rawHooks) > 0 {
		hooksJSON, err := json.Marshal(rawHooks)
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
		return fmt.Errorf("failed to marshal %s: %w", HooksFileName, err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (k *KiroAgent) AreHooksInstalled(ctx context.Context) bool {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}
	hooksPath := filepath.Join(worktreeRoot, ".kiro", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return false
	}

	var hooksFile KiroHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		return false
	}

	return hasEntireHook(hooksFile.Hooks.AgentSpawn) ||
		hasEntireHook(hooksFile.Hooks.UserPromptSubmit) ||
		hasEntireHook(hooksFile.Hooks.Stop) ||
		hasEntireHook(hooksFile.Hooks.PreToolUse) ||
		hasEntireHook(hooksFile.Hooks.PostToolUse)
}

// parseKiroHookType parses a specific hook type from rawHooks into the target slice.
// Silently ignores parse errors (leaves target unchanged).
func parseKiroHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]KiroHookEntry) {
	if data, ok := rawHooks[hookType]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalKiroHookType marshals a hook type back into rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalKiroHookType(rawHooks map[string]json.RawMessage, hookType string, entries []KiroHookEntry) {
	if len(entries) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[hookType] = data
}

// Helper functions for hook management

func hookCommandExists(entries []KiroHookEntry, command string) bool {
	for _, entry := range entries {
		if entry.Command == command {
			return true
		}
	}
	return false
}

func isEntireHook(command string) bool {
	for _, prefix := range entireHookPrefixes {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	return false
}

func hasEntireHook(entries []KiroHookEntry) bool {
	for _, entry := range entries {
		if isEntireHook(entry.Command) {
			return true
		}
	}
	return false
}

func removeEntireHooks(entries []KiroHookEntry) []KiroHookEntry {
	result := make([]KiroHookEntry, 0, len(entries))
	for _, entry := range entries {
		if !isEntireHook(entry.Command) {
			result = append(result, entry)
		}
	}
	return result
}
