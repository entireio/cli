package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure AntigravityAgent implements HookSupport and declares where its hook
// config lives (HookConfigRelPath in antigravity.go), so doctor's symlink scan
// and the vouchable-directory guard cover .agents/hooks.json like every other
// agent's config.
var (
	_ agent.HookSupport       = (*AntigravityAgent)(nil)
	_ agent.HookConfigLocator = (*AntigravityAgent)(nil)
)

// AgentsHooksFileName is the hooks file used by Antigravity.
const AgentsHooksFileName = "hooks.json"

// InstallHooks installs Antigravity hooks in .agents/hooks.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (a *AntigravityAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	cfg, err := a.hookConfig(ctx)
	if err != nil {
		return 0, err
	}

	// Read and parse existing hooks file, preserving unknown keys
	rawFile := make(map[string]json.RawMessage)
	existingData, readErr := cfg.Read()
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing hooks.json: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return 0, readErr //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}

	// Build the candidate Entire hook config. The whole "entire" entry is
	// replaced on install, so a hook written by an older version cannot
	// survive alongside the current one.
	candidate := buildEntireHookConfig()

	// Title tee: agy's only token-usage surface (same payload as the
	// statusline script). Run this BEFORE the idempotency early-return: the
	// title slot lives in agy's GLOBAL settings.json, independent of this
	// repo's .agents/hooks.json. If repo hooks already match but the global
	// slot is missing or stale (upgrade from a pre-title-tee version, a failed
	// first install, or `entire agent add` without --force), re-running setup
	// must still repair it — otherwise the doctor's "re-run setup" hint is a
	// no-op. InstallTitleTee is itself idempotent. Best-effort: a failure to
	// claim the global slot must not fail repo-level hook setup.
	//
	// Gated on agy actually being on PATH. The slot lives in agy's global
	// settings.json, so claiming it from a machine that has never run agy
	// writes a shared user-level file on the strength of a repo-local
	// command — `entire agent add antigravity` in a teammate's checkout, say.
	// `entire doctor` gates its matching check the same way. The order above
	// is unchanged: this still runs before the idempotency early-return, so
	// stale-slot repair keeps working wherever agy is installed.
	if _, lookErr := exec.LookPath(antigravityBinaryName); lookErr != nil {
		logging.Debug(ctx, "skipping antigravity title tee: agy is not on PATH",
			"error", lookErr.Error())
	} else if err := InstallTitleTee(); err != nil {
		logging.Warn(ctx, "failed to install antigravity title tee",
			"error", err.Error())
	}

	// Idempotency check: an entry the user disabled or one that already matches
	// the candidate is left alone. --force remains the explicit override.
	if !force {
		if existing, ok := rawFile["entire"]; ok {
			disabled, same := entireEntryMatches(existing, candidate)
			if disabled || same {
				return 0, nil
			}
		}
	}

	// Marshal and insert the "entire" entry (replacing any prior value)
	candidateBytes, err := jsonutil.MarshalWithNoHTMLEscape(candidate)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hook config: %w", err)
	}
	rawFile["entire"] = candidateBytes

	if err := writeHooksFile(rawFile, cfg); err != nil {
		return 0, err
	}

	// 3 hooks: pre-tool-use, pre-invocation, stop
	return 3, nil
}

// entireEntryMatches compares an installed "entire" entry against candidate by
// re-marshaling both to compact JSON. disabled reports a user-set
// "enabled": false — agy's documented per-entry disable knob, a deliberate
// choice that install must not rewrite and silently re-arm.
func entireEntryMatches(existing json.RawMessage, candidate HookConfig) (disabled, same bool) {
	var existingCfg HookConfig
	if err := json.Unmarshal(existing, &existingCfg); err != nil {
		return false, false
	}
	if existingCfg.Enabled != nil && !*existingCfg.Enabled {
		return true, false
	}
	existingBytes, err1 := jsonutil.MarshalWithNoHTMLEscape(existingCfg)
	candidateBytes, err2 := jsonutil.MarshalWithNoHTMLEscape(candidate)
	return false, err1 == nil && err2 == nil && bytes.Equal(existingBytes, candidateBytes)
}

// HooksEntryMatchesHost reports whether the repo's installed "entire" entry is
// exactly what InstallHooks would write on THIS host. installed is false when
// there is no entry. A user-disabled entry counts as current.
//
// It exists for `entire doctor`: the hook command's SHAPE is host-specific
// (agy runs it through cmd.exe on Windows and sh elsewhere), and a hooks.json
// committed from a macOS checkout carries a sh wrapper that cmd.exe tears
// apart — the hook exits 1, the failure shows only in agy's log, and nothing
// is tracked. A file that merely exists proves nothing about that; comparing
// against the candidate does, at zero cost and without spawning agy.
func (a *AntigravityAgent) HooksEntryMatchesHost(ctx context.Context) (installed, current bool, err error) {
	cfg, err := a.hookConfig(ctx)
	if err != nil {
		return false, false, err
	}
	existing, ok, err := readEntireEntry(cfg)
	if err != nil || !ok {
		return false, false, err
	}
	disabled, same := entireEntryMatches(existing, buildEntireHookConfig())
	return true, disabled || same, nil
}

// readEntireEntry returns the raw "entire" entry from the repo's hooks.json.
// ok is false when the file or the entry is absent, which every caller reads
// as "no Entire hooks here" rather than as an error.
//
// Parsed per-entry, not as a whole file of HookConfigs: foreign entries are
// free-form user content and need not match our struct shapes, and a strict
// whole-file unmarshal would fail on them and permanently report our own
// entry as missing.
func readEntireEntry(cfg *agent.HookConfigFile) (json.RawMessage, bool, error) {
	data, err := cfg.Read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}
	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return nil, false, fmt.Errorf("parse hook config: %w", err)
	}
	entry, ok := rawFile["entire"]
	return entry, ok, nil
}

// HooksDisabled reports whether the repo's "entire" entry is present but
// explicitly switched off with "enabled": false. InstallHooks already treats
// that as a deliberate opt-out and leaves the entry alone.
//
// It exists so `entire doctor` can stay quiet about a configuration the user
// turned off. AreHooksInstalled and DetectPresence deliberately still report
// such an entry as present: they drive agent auto-detection and
// `entire agent list`, which describe what is on disk.
func (a *AntigravityAgent) HooksDisabled(ctx context.Context) (bool, error) {
	cfg, err := a.hookConfig(ctx)
	if err != nil {
		return false, err
	}
	entry, ok, err := readEntireEntry(cfg)
	if err != nil || !ok {
		return false, err
	}
	var existingCfg HookConfig
	if err := json.Unmarshal(entry, &existingCfg); err != nil {
		return false, fmt.Errorf("parse entire hook entry: %w", err)
	}
	return existingCfg.Enabled != nil && !*existingCfg.Enabled, nil
}

// hookConfig opens the repo's .agents/hooks.json through agent.HookConfigFile,
// which anchors on the worktree root and refuses a symlink at any component
// it creates directories under or writes through.
func (a *AntigravityAgent) hookConfig(ctx context.Context) (*agent.HookConfigFile, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Not a repository (tests, and `enable` before `git init`): the process
		// directory is the only candidate, and it is a directory the caller
		// chose rather than one derived from anything read off disk. The same
		// fallback every other agent's hook config uses.
		repoRoot = "."
	}
	return agent.OpenHookConfig(repoRoot, a.HookConfigRelPath()) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// UninstallHooks removes the Entire hook entry from .agents/hooks.json.
func (a *AntigravityAgent) UninstallHooks(ctx context.Context) error {
	cfg, err := a.hookConfig(ctx)
	if err != nil {
		return err
	}
	data, err := cfg.Read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // No hooks file means nothing to uninstall
		}
		return err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return fmt.Errorf("failed to parse hooks.json: %w", err)
	}

	// Nothing of ours in the file: leave it byte-for-byte alone. Rewriting it
	// would re-indent and reorder a file that holds only the user's own
	// entries, for no change of Entire's (the title-tee uninstall is careful
	// about exactly this).
	if _, ok := rawFile["entire"]; !ok {
		return nil
	}
	delete(rawFile, "entire")

	return writeHooksFile(rawFile, cfg)
}

// AreHooksInstalled checks if Entire hooks are installed.
func (a *AntigravityAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	hookCfg, err := a.hookConfig(ctx)
	if err != nil {
		return false, err
	}
	entireRaw, ok, err := readEntireEntry(hookCfg)
	if err != nil || !ok {
		return false, err
	}
	var cfg HookConfig
	if err := json.Unmarshal(entireRaw, &cfg); err != nil {
		return false, fmt.Errorf("parse entire hook entry: %w", err)
	}

	// Check at least one of our hook commands is present
	return hasEntireHookInToolHandlers(cfg.PreToolUse) ||
		hasEntireHookInToolHandlers(cfg.PostToolUse) ||
		hasEntireHookInSimpleHandlers(cfg.PreInvocation) ||
		hasEntireHookInSimpleHandlers(cfg.PostInvocation) ||
		hasEntireHookInSimpleHandlers(cfg.Stop), nil
}

// stopHookTimeoutSeconds is the explicit timeout installed on the Stop
// handler. Stop runs PrepareTranscript (short bounded wait) plus SaveStep — a
// shadow-branch checkpoint write that can exceed agy's 30s default timeout on
// large repos, in which case agy kills the hook mid-checkpoint with no trace.
const stopHookTimeoutSeconds = 300

// buildEntireHookConfig constructs the HookConfig for the "entire" entry for
// the host this binary runs on.
func buildEntireHookConfig() HookConfig {
	return buildEntireHookConfigForHost(agent.HookHostIsWindows())
}

// buildEntireHookConfigForHost constructs the "entire" entry for a Windows or
// POSIX hook host. agy hands every hook command to cmd.exe /C on Windows
// whatever else is installed (a Git Bash sh on PATH changes nothing), so the
// gate is agent.HookHostIsWindows, not the UseWindowsProductionHooks sh probe,
// and the wrapper is the bare direct-shell form: the sh wrapper is cut apart by
// cmd.exe (`>/dev/null` becomes a redirect to a missing path) and the nested
// cmd.exe form becomes one unrecognised program name. Both fail the hook with
// exit 1, visible only in agy's own log while the turn reports SUCCESS —
// silent, total loss of tracking (trail 444, confirmed on Windows 11 ARM64 with
// agy 1.2.7).
func buildEntireHookConfigForHost(windowsHost bool) HookConfig {
	const cmdPrefix = "entire hooks antigravity "
	makeCmd := func(verb string) string {
		if windowsHost {
			return agent.WrapWindowsProductionSilentHookCommandDirect(cmdPrefix + verb)
		}
		return agent.WrapProductionSilentHookCommand(cmdPrefix + verb)
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
				Hooks:   []HookCommand{{Type: hookTypeCommand, Command: makeCmd("pre-tool-use")}},
			},
		},
		PreInvocation: []SimpleHandler{{Type: hookTypeCommand, Command: makeCmd("pre-invocation")}},
		Stop:          []SimpleHandler{{Type: hookTypeCommand, Command: makeCmd("stop"), Timeout: stopHookTimeoutSeconds}},
	}
}

// writeHooksFile marshals rawFile and writes it through cfg, which creates the
// parent directories inside the worktree root and refuses symlinks.
func writeHooksFile(rawFile map[string]json.RawMessage, cfg *agent.HookConfigFile) error {
	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	return cfg.Write(output, 0o600) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// hasEntireHookInToolHandlers checks if any ToolHandler entry is an Entire hook.
func hasEntireHookInToolHandlers(handlers []ToolHandler) bool {
	for _, th := range handlers {
		for _, hc := range th.Hooks {
			if agent.IsManagedHookCommand(hc.Command) {
				return true
			}
		}
	}
	return false
}

// hasEntireHookInSimpleHandlers checks if any SimpleHandler entry is an Entire hook.
func hasEntireHookInSimpleHandlers(handlers []SimpleHandler) bool {
	for _, sh := range handlers {
		if agent.IsManagedHookCommand(sh.Command) {
			return true
		}
	}
	return false
}
