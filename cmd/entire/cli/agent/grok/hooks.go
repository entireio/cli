package grok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Hook verbs. These become subcommands under `entire hooks grok`.
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNameStop             = "stop"
	HookNameStopCancelled    = "stop-cancelled"
	HookNameStopFailure      = "stop-failure"
	HookNamePreCompact       = "pre-compact"
	HookNameSubagentStart    = "subagent-start"
	HookNameSubagentStop     = "subagent-stop"
	HookNamePostToolUse      = "post-tool-use"
)

// hooksFileRelPath is the file Entire owns outright.
//
// Grok merges every *.json under .grok/hooks/, so Entire gets its own file
// rather than editing one a user also maintains. Install is a whole-file write
// and uninstall is a delete — no read-modify-write, and no way to clobber a
// user's own hooks.
var hooksFileRelPath = filepath.Join(".grok", "hooks", "entire.json")

// entireMarker identifies the file as Entire-owned.
const entireMarker = "entire hooks grok "

// grokEventForHook maps an Entire hook verb to the PascalCase event name Grok
// expects as a config key. The stdin payload spells the same event in
// snake_case; see types.go.
var grokEventForHook = map[string]string{
	HookNameSessionStart:     "SessionStart",
	HookNameSessionEnd:       "SessionEnd",
	HookNameUserPromptSubmit: "UserPromptSubmit",
	HookNameStop:             "Stop",
	HookNameStopCancelled:    "StopCancelled",
	HookNameStopFailure:      "StopFailure",
	HookNamePreCompact:       "PreCompact",
	HookNameSubagentStart:    "SubagentStart",
	HookNameSubagentStop:     "SubagentStop",
	HookNamePostToolUse:      "PostToolUse",
}

// hookInstallOrder keeps the rendered config deterministic so
// GeneratedHookFileState can compare it byte-for-byte.
var hookInstallOrder = []string{
	HookNameSessionStart,
	HookNameUserPromptSubmit,
	HookNamePostToolUse,
	HookNameStop,
	HookNameStopCancelled,
	HookNameStopFailure,
	HookNameSubagentStart,
	HookNameSubagentStop,
	HookNamePreCompact,
	HookNameSessionEnd,
}

// hookTimeoutSeconds bounds the non-gate hooks. Grok defaults those to 5s,
// which is tight for a cold `entire` invocation on a large repo, so ask for
// more.
const hookTimeoutSeconds = 30

// turnEndTimeoutSeconds is the budget for hooks that end a turn.
//
// Turn end is where the expensive work happens — redaction, condensation, the
// checkpoint write — and on a large repo it can exceed any short bound. Grok
// fails open on timeout, so a budget that is too small does not surface an
// error: the checkpoint is simply dropped. 600s matches what Grok already
// allows its own blocking gates.
const turnEndTimeoutSeconds = 600

// hookTimeout returns the timeout to write for a verb. Zero means "omit the
// field", leaving Grok's own default in place.
//
// Three tiers, because Grok's defaults are not uniform:
//
//   - Stop and SubagentStop are blocking gates Grok already defaults to 600s.
//     Writing anything here would only lower that, so the field is omitted.
//   - StopCancelled and StopFailure end a turn too — they map to TurnEnd and
//     run the identical checkpoint path — but Grok treats them as ordinary
//     events with a 5s default, so they need the long budget stated
//     explicitly. Omitting it would leave them at 5s, which is worse than the
//     30s this originally shipped with.
//   - Everything else is observational and comfortably fits the shorter bound.
func hookTimeout(verb string) int {
	switch verb {
	case HookNameStop, HookNameSubagentStop:
		return 0
	case HookNameStopCancelled, HookNameStopFailure:
		return turnEndTimeoutSeconds
	default:
		return hookTimeoutSeconds
	}
}

// HookNames returns the hook verbs Grok supports.
func (g *GrokAgent) HookNames() []string {
	out := make([]string, len(hookInstallOrder))
	copy(out, hookInstallOrder)
	return out
}

// GetSupportedHooks returns the hook types Grok supports.
func (g *GrokAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookSessionEnd,
		agent.HookUserPromptSubmit,
		agent.HookStop,
		agent.HookPostToolUse,
	}
}

// grokHookCommand is one command entry inside a matcher group.
type grokHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// grokHookGroup is one matcher group for an event.
type grokHookGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []grokHookCommand `json:"hooks"`
}

// grokHooksFile is the whole config. Grok reuses Claude Code's nested shape
// verbatim.
type grokHooksFile struct {
	Hooks map[string][]grokHookGroup `json:"hooks"`
}

func hooksPath(ctx context.Context) (string, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		//nolint:forbidigo // explicit fallback when WorktreeRoot fails
		root, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve repo root: %w", err)
		}
	}
	return filepath.Join(root, hooksFileRelPath), nil
}

// renderHooks returns the full config content Entire installs.
func renderHooks(ctx context.Context) (string, error) {
	useWindowsHooks := agent.UseWindowsProductionHooks(ctx)

	file := grokHooksFile{Hooks: map[string][]grokHookGroup{}}
	for _, verb := range hookInstallOrder {
		event, ok := grokEventForHook[verb]
		if !ok {
			continue
		}
		cmd := agent.WrapProductionSilentHookCommandForOS(entireMarker+verb, useWindowsHooks)
		file.Hooks[event] = []grokHookGroup{{
			Matcher: "",
			Hooks:   []grokHookCommand{{Type: "command", Command: cmd, Timeout: hookTimeout(verb)}},
		}}
	}

	out, err := jsonutil.MarshalIndentWithNewline(file, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal grok hooks: %w", err)
	}
	return string(out), nil
}

// InstallHooks writes .grok/hooks/entire.json.
// Returns 1 when the file was written, 0 when it was already current.
//
// A file at that path that is not recognisably Entire's is left alone unless
// force is set, so a user who happens to keep their own entire.json there does
// not lose it.
func (g *GrokAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	path, err := hooksPath(ctx)
	if err != nil {
		return 0, err
	}
	content, err := renderHooks(ctx)
	if err != nil {
		return 0, err
	}

	if !force {
		existing, readErr := os.ReadFile(path) //nolint:gosec // path from validated repo root
		switch {
		case readErr == nil && string(existing) == content:
			return 0, nil
		case readErr == nil && !strings.Contains(string(existing), entireMarker):
			return 0, fmt.Errorf("refusing to overwrite foreign file at %s; remove it or pass --force", path)
		}
	}

	//nolint:gosec // G301: grok reads the directory; standard 0755 permissions
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("create grok hooks dir: %w", err)
	}
	//nolint:gosec // G306: grok reads the file; standard 0644 permissions
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return 0, fmt.Errorf("write grok hooks: %w", err)
	}
	return 1, nil
}

// UninstallHooks removes Entire's hooks file. A foreign file at that path is
// left in place.
func (g *GrokAgent) UninstallHooks(ctx context.Context) error {
	path, err := hooksPath(ctx)
	if err != nil {
		return err
	}
	data, readErr := os.ReadFile(path) //nolint:gosec // path from validated repo root
	if readErr != nil {
		return nil //nolint:nilerr // nothing installed means nothing to remove
	}
	if !strings.Contains(string(data), entireMarker) {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove grok hooks: %w", err)
	}
	return nil
}

// AreHooksInstalled reports whether Entire's hooks file is present.
//
// A missing hook file is a definite "no hooks"; anything that stops the file
// from being read is reported as an error so callers can distinguish "absent"
// from "unknown" (see agent.HookSupport).
func (g *GrokAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	path, err := hooksPath(ctx)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path from validated repo root
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read grok hook file: %w", err)
	}
	return strings.Contains(string(data), entireMarker), nil
}

// CheckHookConfig reports whether the installed config matches what
// InstallHooks would write today.
//
// This matters for Grok specifically: .grok/hooks/entire.json is a repo-local
// file teams commonly commit so every clone is checkpointed without each person
// running enable. A committed config goes stale as hook verbs change, and
// AreHooksInstalled keeps returning true because the marker is still there —
// while Grok's own hook execution fails open, so nothing surfaces the drift.
func (g *GrokAgent) CheckHookConfig(ctx context.Context) agent.HookConfigState {
	path, err := hooksPath(ctx)
	if err != nil {
		return agent.HooksAbsent
	}
	content, err := renderHooks(ctx)
	if err != nil {
		return agent.HooksAbsent
	}
	return agent.GeneratedHookFileState(path, entireMarker, content)
}
