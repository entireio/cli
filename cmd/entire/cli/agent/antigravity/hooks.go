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
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure AntigravityAgent implements HookSupport
var _ agent.HookSupport = (*AntigravityAgent)(nil)

// AgentsHooksFileName is the hooks file used by Antigravity.
const AgentsHooksFileName = "hooks.json"

// entireHookPrefixes are command prefixes that identify Entire hooks.
var entireHookPrefixes = []string{
	"entire hooks antigravity ",
	"go run ",
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

	// 5 hooks: pre-tool-use, post-tool-use, pre-invocation, post-invocation, stop
	return 5, nil
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

	return HookConfig{
		PreToolUse: []ToolHandler{
			{
				Matcher: "*",
				Hooks:   []HookCommand{{Type: "command", Command: makeCmd("pre-tool-use")}},
			},
		},
		PostToolUse: []ToolHandler{
			{
				Matcher: "*",
				Hooks:   []HookCommand{{Type: "command", Command: makeCmd("post-tool-use")}},
			},
		},
		PreInvocation:  []SimpleHandler{{Type: "command", Command: makeCmd("pre-invocation")}},
		PostInvocation: []SimpleHandler{{Type: "command", Command: makeCmd("post-invocation")}},
		Stop:           []SimpleHandler{{Type: "command", Command: makeCmd("stop")}},
	}
}

// writeHooksFile marshals rawFile and writes it to hooksPath, creating
// parent directories as needed.
func writeHooksFile(rawFile map[string]json.RawMessage, hooksPath string) error {
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return fmt.Errorf("failed to create .agents directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write hooks.json: %w", err)
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
