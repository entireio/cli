package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure AntigravityAgent implements HookSupport
var _ agent.HookSupport = (*AntigravityAgent)(nil)

// AgentsHooksFileName is the hooks file used by Antigravity.
const AgentsHooksFileName = "hooks.json"

// entireHookPrefixes are command prefixes that identify Entire hooks. The
// localDev prefix is the full canonical form (matching cursor/claudecode) —
// a bare "go run " prefix would misclassify any user-authored go-run command
// under the "entire" hooks key as Entire-managed.
var entireHookPrefixes = []string{
	"entire hooks antigravity ",
	`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go `,
}

// InstallHooks installs Antigravity hooks in .agents/hooks.json.
// If localDev is true, hooks point to the local development build.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (a *AntigravityAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	hooksPath := filepath.Join(repoRoot, ".agents", AgentsHooksFileName)

	// Read and parse existing hooks file, preserving unknown keys
	rawFile := make(map[string]json.RawMessage)
	existingData, readErr := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing hooks.json: %w", err)
		}
	}

	// Build the candidate Entire hook config
	var cmdPrefix string
	if localDev {
		cmdPrefix = `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks antigravity `
	} else {
		cmdPrefix = "entire hooks antigravity "
	}

	candidate := buildEntireHookConfig(cmdPrefix, localDev)

	// Title tee: agy's only token-usage surface (same payload as the
	// statusline script). Run this BEFORE the idempotency early-return: the
	// title slot lives in agy's GLOBAL settings.json, independent of this
	// repo's .agents/hooks.json. If repo hooks already match but the global
	// slot is missing or stale (upgrade from a pre-title-tee version, a failed
	// first install, or `entire agent add` without --force), re-running setup
	// must still repair it — otherwise the doctor's "re-run setup" hint is a
	// no-op. InstallTitleTee is itself idempotent. Best-effort: a failure to
	// claim the global slot must not fail repo-level hook setup.
	if err := InstallTitleTee(localDev); err != nil {
		logging.Warn(ctx, "failed to install antigravity title tee",
			"error", err.Error())
	}

	// Idempotency check: compare candidate against existing "entire" entry by
	// re-marshaling both to compact JSON for a stable comparison.
	if !force {
		if existing, ok := rawFile["entire"]; ok {
			var existingCfg HookConfig
			if err := json.Unmarshal(existing, &existingCfg); err == nil {
				existingBytes, err1 := jsonutil.MarshalWithNoHTMLEscape(existingCfg)
				candidateBytes, err2 := jsonutil.MarshalWithNoHTMLEscape(candidate)
				if err1 == nil && err2 == nil && bytes.Equal(existingBytes, candidateBytes) {
					return 0, nil
				}
			}
		}
	}

	// Marshal and insert the "entire" entry (replacing any prior value)
	candidateBytes, err := jsonutil.MarshalWithNoHTMLEscape(candidate)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hook config: %w", err)
	}
	rawFile["entire"] = candidateBytes

	if err := writeHooksFile(rawFile, hooksPath); err != nil {
		return 0, err
	}

	// 3 hooks: pre-tool-use, pre-invocation, stop
	return 3, nil
}

// UninstallHooks removes the Entire hook entry from .agents/hooks.json.
func (a *AntigravityAgent) UninstallHooks(ctx context.Context) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}

	hooksPath := filepath.Join(repoRoot, ".agents", AgentsHooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No hooks file means nothing to uninstall
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return fmt.Errorf("failed to parse hooks.json: %w", err)
	}

	delete(rawFile, "entire")

	return writeHooksFile(rawFile, hooksPath)
}

// AreHooksInstalled checks if Entire hooks are installed.
func (a *AntigravityAgent) AreHooksInstalled(ctx context.Context) bool {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}

	hooksPath := filepath.Join(repoRoot, ".agents", AgentsHooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return false
	}

	var f HooksFile
	if err := json.Unmarshal(data, &f); err != nil {
		return false
	}

	cfg, ok := f["entire"]
	if !ok {
		return false
	}

	// Check at least one of our hook commands is present
	return hasEntireHookInToolHandlers(cfg.PreToolUse) ||
		hasEntireHookInToolHandlers(cfg.PostToolUse) ||
		hasEntireHookInSimpleHandlers(cfg.PreInvocation) ||
		hasEntireHookInSimpleHandlers(cfg.PostInvocation) ||
		hasEntireHookInSimpleHandlers(cfg.Stop)
}

// buildEntireHookConfig constructs the HookConfig for the "entire" entry.
func buildEntireHookConfig(cmdPrefix string, localDev bool) HookConfig {
	makeCmd := func(verb string) string {
		cmd := cmdPrefix + verb
		if !localDev {
			cmd = agent.WrapProductionSilentHookCommand(cmd)
		}
		return cmd
	}

	// PostToolUse and PostInvocation are deliberately NOT installed: neither
	// maps to a lifecycle event, and installing them spawns a no-op `entire`
	// subprocess on every completed tool call / model invocation. The struct
	// fields stay in HookConfig so the idempotency comparison detects (and
	// replaces) stale installs that still carry them.
	return HookConfig{
		PreToolUse: []ToolHandler{
			{
				Matcher: "*",
				Hooks:   []HookCommand{{Type: "command", Command: makeCmd("pre-tool-use")}},
			},
		},
		PreInvocation: []SimpleHandler{{Type: "command", Command: makeCmd("pre-invocation")}},
		Stop:          []SimpleHandler{{Type: "command", Command: makeCmd("stop")}},
	}
}

// writeHooksFile marshals rawFile and writes it to hooksPath, creating
// parent directories as needed.
func writeHooksFile(rawFile map[string]json.RawMessage, hooksPath string) error {
	return writeJSONMapFile(rawFile, hooksPath, "hooks.json")
}

// writeJSONMapFile marshals a raw JSON map with indentation and writes it to
// path, creating parent directories as needed. Shared by the repo-level
// hooks.json writer and the global agy settings.json writer.
func writeJSONMapFile(rawFile map[string]json.RawMessage, path, what string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create directory for %s: %w", what, err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal %s: %w", what, err)
	}

	if err := os.WriteFile(path, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", what, err)
	}
	return nil
}

// hasEntireHookInToolHandlers checks if any ToolHandler entry is an Entire hook.
func hasEntireHookInToolHandlers(handlers []ToolHandler) bool {
	for _, th := range handlers {
		for _, hc := range th.Hooks {
			if agent.IsManagedHookCommand(hc.Command, entireHookPrefixes) {
				return true
			}
		}
	}
	return false
}

// hasEntireHookInSimpleHandlers checks if any SimpleHandler entry is an Entire hook.
func hasEntireHookInSimpleHandlers(handlers []SimpleHandler) bool {
	for _, sh := range handlers {
		if agent.IsManagedHookCommand(sh.Command, entireHookPrefixes) {
			return true
		}
	}
	return false
}
