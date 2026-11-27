package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// HooksFileName is the hooks config file used by Codex.
const HooksFileName = "hooks.json"

const maxHooksFileBytes = 1 << 20

// defaultHookTimeoutSec is the timeout Entire configures for Codex hooks that
// run between turns, where Codex allows up to its standard 600s.
const defaultHookTimeoutSec = 30

// HookEventSpec is the shared Codex event metadata used by installation,
// trust inspection, and the Codex integration tests.
type HookEventSpec struct {
	Event       string
	Label       string
	Verb        string
	Timeout     int
	Managed     bool
	Core        bool
	JSONWarning bool
}

var hookEventSpecs = []HookEventSpec{
	{Event: "SessionStart", Label: "session_start", Verb: HookNameSessionStart, Timeout: defaultHookTimeoutSec, Managed: true, Core: true, JSONWarning: true},
	{Event: "SessionEnd", Label: "session_end", Verb: HookNameSessionEnd, Timeout: SessionEndTimeoutSec, Managed: true},
	{Event: "UserPromptSubmit", Label: "user_prompt_submit", Verb: HookNameUserPromptSubmit, Timeout: defaultHookTimeoutSec, Managed: true, Core: true},
	{Event: "Stop", Label: "stop", Verb: HookNameStop, Timeout: defaultHookTimeoutSec, Managed: true, Core: true},
	{Event: "PostToolUse", Label: "post_tool_use", Verb: HookNamePostToolUse, Timeout: defaultHookTimeoutSec, Managed: true, Core: true},
	{Event: "SubagentStart", Label: "subagent_start", Verb: HookNameSubagentStart, Timeout: defaultHookTimeoutSec, Managed: true},
	{Event: "SubagentStop", Label: "subagent_stop", Verb: HookNameSubagentStop, Timeout: defaultHookTimeoutSec, Managed: true},
	{Event: "PreToolUse", Label: "pre_tool_use"},
	{Event: "PermissionRequest", Label: "permission_request"},
	{Event: "PreCompact", Label: "pre_compact"},
	{Event: "PostCompact", Label: "post_compact"},
}

// HookEventSpecs returns a copy so callers cannot mutate the canonical table.
func HookEventSpecs() []HookEventSpec {
	return append([]HookEventSpec(nil), hookEventSpecs...)
}

// managedHook adds the command wrappers to one canonical event specification.
type managedHook struct {
	event   string // hooks.json key
	label   string // Codex trust-state key
	verb    string // `entire hooks codex <verb>`
	timeout int

	// posix wraps the hook for `command`, which Codex runs through sh on macOS
	// and Linux. windows wraps it for `commandWindows`, which Codex runs in its
	// place on native Windows. Both are always written: Codex chooses between
	// them by the platform it runs on, so Entire never has to guess that shell
	// from the one `entire enable` happened to run in.
	posix   func(cmd string) string
	windows func(cmd string) string
	// legacyWindows is what releases before commandWindows wrote into
	// `command` on a Windows host whose sh probe failed. Recognized, never
	// written.
	legacyWindows func(cmd string) string

	// core marks the events whose absence means Codex was never enabled in this
	// repo, as opposed to enabled against an older release that installed fewer
	// events. Only these gate AreHooksInstalled — see the comment there.
	core bool
}

// commands returns the command and commandWindows Entire installs for h.
func (h managedHook) commands() (posix, windows string) {
	const cmdPrefix = "entire hooks codex "
	return h.posix(cmdPrefix + h.verb), h.windows(cmdPrefix + h.verb)
}

func buildManagedHooks() []managedHook {
	managed := make([]managedHook, 0, len(hookEventSpecs))
	for _, spec := range hookEventSpecs {
		if !spec.Managed {
			continue
		}
		posix := agent.WrapProductionSilentHookCommand
		windows := agent.WrapWindowsProductionSilentHookCommand
		legacyWindows := agent.WrapWindowsProductionSilentHookCommand
		if spec.JSONWarning {
			posix = func(cmd string) string {
				return agent.WrapProductionJSONWarningHookCommand(cmd, agent.WarningFormatSingleLine)
			}
			windows = func(cmd string) string {
				return agent.WrapWindowsProductionNestedPlainTextWarningHookCommand(cmd, agent.WarningFormatSingleLine)
			}
			legacyWindows = func(cmd string) string {
				return agent.WrapWindowsProductionJSONWarningHookCommand(cmd, agent.WarningFormatSingleLine)
			}
		}
		managed = append(managed, managedHook{
			event: spec.Event, label: spec.Label, verb: spec.Verb,
			timeout: spec.Timeout, core: spec.Core,
			posix: posix, windows: windows, legacyWindows: legacyWindows,
		})
	}
	return managed
}

var managedHooks = buildManagedHooks()

// InstallHooks installs Codex hooks in .codex/hooks.json.
func (c *CodexAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return 0, err
	}
	if err := validateMutableHookTarget(worktreeHooks); err != nil {
		return 0, err
	}
	cfg, err := worktreeHooks.hookConfig()
	if err != nil {
		return 0, err
	}
	existingData, exists, err := readHooksFileForMutation(cfg)
	if err != nil {
		return 0, err
	}
	var rawHooks map[string]json.RawMessage
	if exists {
		var hooksFile map[string]json.RawMessage
		if err := json.Unmarshal(existingData, &hooksFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing hooks.json: %w", err)
		}
		if hooksRaw, ok := hooksFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in hooks.json: %w", err)
			}
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	count := 0
	updated := make([][]MatcherGroup, len(managedHooks))
	for i, h := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, h.event, &groups); err != nil {
			return 0, err
		}
		if force {
			groups = removeEntireHooks(groups)
		}
		if synced, changed := syncHookCommand(groups, managedEntry(h)); changed {
			groups = synced
			count++
		}
		updated[i] = groups
	}
	if count == 0 {
		return 0, nil
	}
	for i, h := range managedHooks {
		marshalHookType(rawHooks, h.event, updated[i])
	}

	topLevel := make(map[string]json.RawMessage)
	if exists {
		_ = json.Unmarshal(existingData, &topLevel) //nolint:errcheck // parsed above
	}
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	topLevel["hooks"] = hooksJSON
	if err := validateMutableHookTarget(worktreeHooks); err != nil {
		return 0, err
	}
	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	// Write creates .codex with MkdirAllNoSymlink, anchored on the worktree
	// root rather than on .codex itself.
	if err := cfg.Write(output, 0o600); err != nil {
		return 0, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}

	// No .codex/config.toml is written: hooks are enabled by default in
	// Codex (since 0.124.0), and a TOML file inside Codex's reserved
	// <CODEX_HOME>/agents tree would be rejected by its agent-role scanner
	// at every startup (entireio/cli#842). A leftover config.toml written
	// by an older entire version must be removed manually.
	return count, nil
}

// UninstallHooks removes Entire hooks from Codex hooks.json.
func (c *CodexAgent) UninstallHooks(ctx context.Context) error {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return err
	}
	if err := validateMutableHookTarget(worktreeHooks); err != nil {
		return err
	}
	cfg, err := worktreeHooks.hookConfig()
	if err != nil {
		return err
	}
	data, exists, err := readHooksFileForMutation(cfg)
	if err != nil {
		// readHooksFileForMutation reports an absent file as !exists with no
		// error, so a real read failure never masquerades as "nothing to
		// uninstall" — that would leave the hooks on disk and report success.
		return err
	}
	if !exists {
		return nil
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return fmt.Errorf("failed to parse hooks.json: %w", err)
	}
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks: %w", err)
		}
	}
	if rawHooks == nil {
		return nil
	}
	for _, h := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, h.event, &groups); err != nil {
			return err
		}
		marshalHookType(rawHooks, h.event, removeEntireHooks(groups))
	}
	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		topLevel["hooks"] = hooksJSON
	} else {
		delete(topLevel, "hooks")
	}
	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	if err := validateMutableHookTarget(worktreeHooks); err != nil {
		return err
	}
	return cfg.Write(output, 0o600) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// AreHooksInstalled reports whether Codex is wired up to Entire in this repo.
//
// It requires only the core events, not everything InstallHooks writes today.
// The two questions are different: this one decides whether Codex is listed as
// an installed agent (`entire status`, the review and investigate pickers), and
// answering it with the full set would drop Codex out of all of them the moment
// a release adds an event — every existing install predates the addition. Drift
// against today's set is part of hook-config inspection, which `entire doctor`
// reports with the fix (`entire enable`).
//
// A missing config file is an answer, not a failure: that file is where the
// state lives, so its absence means no hooks. Anything that stops us reading the
// answer — an unreadable file, malformed config — is returned as an error, since
// "we could not tell" and "there are none" are different things to a caller
// deciding whether hooks can be left alone.
func (c *CodexAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		logging.Warn(ctx, "codex: failed to resolve hooks path", "err", err)
		return false, fmt.Errorf("resolve Codex hooks path: %w", err)
	}
	hooksPath := worktreeHooks.Path()
	if err := validateMutableHookTarget(worktreeHooks); err != nil {
		logging.Warn(ctx, "codex: hooks path is not usable", "path", hooksPath, "err", err)
		return false, err
	}
	cfg, err := worktreeHooks.hookConfig()
	if err != nil {
		return false, err
	}
	data, exists, err := readHooksFileForMutation(cfg)
	if err != nil {
		logging.Warn(ctx, "codex: failed to read hooks file", "path", hooksPath, "err", err)
		return false, err
	}
	if !exists {
		return false, nil
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		logging.Warn(ctx, "codex: failed to parse hooks file", "path", hooksPath, "err", err)
		return false, fmt.Errorf("parse hook config: %w", err)
	}
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			logging.Warn(ctx, "codex: failed to parse hooks object", "path", hooksPath, "err", err)
			return false, fmt.Errorf("parse hook config: %w", err)
		}
	}
	for _, h := range managedHooks {
		if !h.core {
			continue
		}
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, h.event, &groups); err != nil {
			logging.Warn(ctx, "codex: failed to parse hook event", "path", hooksPath, "event", h.event, "err", err)
			return false, fmt.Errorf("parse hook config: %w", err)
		}
		if !hasEntireHook(groups) {
			return false, nil
		}
	}
	return true, nil
}

// CheckHookConfig reports whether the current checkout's Codex hook
// configuration is absent, current, or needs installation.
func (c *CodexAgent) CheckHookConfig(ctx context.Context) agent.HookConfigState {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return agent.HooksAbsent
	}
	inspection := inspectWorktreeHookConfig(ctx, worktreeHooks)
	switch inspection.State {
	case HookFileMalformed, HookFileUnavailable:
		return agent.HooksAbsent
	case HookFileEntire:
		if inspection.Current {
			return agent.HooksCurrent
		}
		return agent.HooksOutdated
	case HookFileAbsent, HookFileUserOnly:
		return agent.HooksAbsent
	}
	return agent.HooksAbsent
}

type managedHookSpec struct {
	event string
	label string
	entry HookEntry
	core  bool
}

// HookFileState distinguishes Entire's installation from unrelated user
// configuration, malformed JSON, and a file that cannot be inspected.
type HookFileState uint8

const (
	HookFileAbsent HookFileState = iota
	HookFileUserOnly
	HookFileEntire
	HookFileMalformed
	HookFileUnavailable
)

// HookConfigInspection is the single parsed view used by Codex presence,
// freshness, missing-hook, and doctor reporting.
type HookConfigInspection struct {
	State         HookFileState
	Missing       []string
	Declared      []string
	Current       bool
	CoreInstalled bool
	Err           error
}

type hooksDocument struct {
	topLevel map[string]json.RawMessage
	rawHooks map[string]json.RawMessage
	exists   bool
}

// managedHookSpecs is the same on every host: the config Entire writes no
// longer depends on the shell `entire enable` or `entire doctor` runs in, so a
// file written from PowerShell reads as current from Git Bash and back.
func managedHookSpecs() []managedHookSpec {
	specs := make([]managedHookSpec, 0, len(managedHooks))
	for _, hook := range managedHooks {
		specs = append(specs, managedHookSpec{
			event: hook.event,
			label: hook.label,
			entry: managedEntry(hook),
			core:  hook.core,
		})
	}
	return specs
}

// managedEntry is the hooks.json entry Entire installs for h.
func managedEntry(h managedHook) HookEntry {
	posix, windows := h.commands()
	return HookEntry{
		Type:           "command",
		Command:        posix,
		CommandWindows: windows,
		Timeout:        h.timeout,
	}
}

func inspectWorktreeHookConfig(ctx context.Context, hooks WorktreeHooksPath) HookConfigInspection {
	projectDir, err := validateWorktreeHookTarget(hooks)
	if err != nil {
		return HookConfigInspection{State: HookFileUnavailable, Err: err}
	}
	if err := validateExistingProjectDir(projectDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HookConfigInspection{State: HookFileAbsent}
		}
		return HookConfigInspection{State: HookFileUnavailable, Err: err}
	}
	return inspectHookConfigAt(ctx, hooks.Path())
}

func inspectDiscoveredHookConfig(ctx context.Context, hooks DiscoveredHooksPath) HookConfigInspection {
	if err := validateDiscoveredHookTarget(hooks); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HookConfigInspection{State: HookFileAbsent}
		}
		return HookConfigInspection{State: HookFileUnavailable, Err: err}
	}
	return inspectHookConfigAt(ctx, hooks.Path())
}

func inspectWorktreeHookConfigLightweight(hooks WorktreeHooksPath) HookConfigInspection {
	projectDir, err := validateWorktreeHookTarget(hooks)
	if err != nil {
		return HookConfigInspection{State: HookFileUnavailable, Err: err}
	}
	if err := validateExistingProjectDir(projectDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HookConfigInspection{State: HookFileAbsent}
		}
		return HookConfigInspection{State: HookFileUnavailable, Err: err}
	}
	return inspectHookConfigLightweightAt(hooks.Path())
}

func inspectDiscoveredHookConfigLightweight(hooks DiscoveredHooksPath) HookConfigInspection {
	if err := validateDiscoveredHookTarget(hooks); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HookConfigInspection{State: HookFileAbsent}
		}
		return HookConfigInspection{State: HookFileUnavailable, Err: err}
	}
	return inspectHookConfigLightweightAt(hooks.Path())
}

func inspectHookConfigLightweightAt(path string) HookConfigInspection {
	document, err := readHooksDocument(path)
	if err != nil {
		return HookConfigInspection{State: hookFileStateForReadError(err), Err: err}
	}
	if !document.exists {
		return HookConfigInspection{State: HookFileAbsent}
	}
	inspection := HookConfigInspection{State: HookFileUserOnly, Current: true, CoreInstalled: true}
	inspection.Declared, err = declaredCodexEventsFromDocument(document)
	if err != nil {
		return HookConfigInspection{State: HookFileMalformed, Err: err}
	}
	for _, hook := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(document.rawHooks, hook.event, &groups); err != nil {
			return HookConfigInspection{State: HookFileMalformed, Err: err}
		}
		if hasEntireHook(groups) {
			inspection.State = HookFileEntire
		} else if hook.core {
			inspection.CoreInstalled = false
		}
	}
	if inspection.State == HookFileUserOnly {
		inspection.Current = false
		inspection.CoreInstalled = false
	}
	return inspection
}

func inspectHookConfigAt(_ context.Context, path string) HookConfigInspection {
	document, err := readHooksDocument(path)
	if err != nil {
		return HookConfigInspection{State: hookFileStateForReadError(err), Err: err}
	}
	if !document.exists {
		return HookConfigInspection{State: HookFileAbsent}
	}

	inspection := HookConfigInspection{
		State:         HookFileUserOnly,
		Current:       true,
		CoreInstalled: true,
	}
	inspection.Declared, err = declaredCodexEventsFromDocument(document)
	if err != nil {
		return HookConfigInspection{State: HookFileMalformed, Err: err}
	}
	for _, spec := range managedHookSpecs() {
		var groups []MatcherGroup
		if err := parseHookType(document.rawHooks, spec.event, &groups); err != nil {
			return HookConfigInspection{State: HookFileMalformed, Err: err}
		}
		installed := hasEntireHook(groups)
		if installed {
			inspection.State = HookFileEntire
		} else {
			inspection.Missing = append(inspection.Missing, spec.label)
			if spec.core {
				inspection.CoreInstalled = false
			}
		}
		if !managedHookIsCurrent(groups, spec.entry) {
			inspection.Current = false
		}
	}
	if inspection.State == HookFileUserOnly {
		inspection.Missing = nil
		inspection.Current = false
		inspection.CoreInstalled = false
	}
	return inspection
}

// hooksDocumentRoot opens the project root containing path and returns
// path's name inside it.
//
// path is always <project-root>/.codex/hooks.json — built that way by every
// caller from a resolved root — so the root is recovered by removing exactly
// that known suffix rather than by taking filepath.Dir. The difference matters:
// Dir() would anchor inside `.codex`, which is the component that needs
// containing. A path that does not have the expected shape is refused rather
// than opened somewhere else.
func hooksDocumentRoot(path string) (*os.Root, string, error) {
	name := filepath.Join(".codex", HooksFileName)
	if !strings.HasSuffix(path, string(filepath.Separator)+name) {
		return nil, "", fmt.Errorf("codex hooks path %q is not a .codex/%s", path, HooksFileName)
	}
	projectRoot := strings.TrimSuffix(path, string(filepath.Separator)+name)
	cfg, err := agent.OpenHookConfig(projectRoot, (&CodexAgent{}).HookConfigRelPath())
	if err != nil {
		return nil, "", err //nolint:wrapcheck // caller names the path it asked about
	}
	root, rootName := cfg.Root()
	return root, rootName, nil
}

func readHooksDocument(path string) (*hooksDocument, error) {
	document := &hooksDocument{
		topLevel: make(map[string]json.RawMessage),
		rawHooks: make(map[string]json.RawMessage),
	}
	// Anchored on the project root, with ".codex/hooks.json" as the name
	// inside it — not on filepath.Dir(path), which would put `.codex` ABOVE the
	// root and leave the containment covering only the final component. A root
	// is only as trustworthy as the directory it is opened on, and `.codex`
	// arrives with the checkout.
	root, name, err := hooksDocumentRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Codex project directory for %q: %w", path, err)
	}
	file, err := osroot.OpenNoFollow(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Codex hooks file %q: %w", path, err)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened Codex hooks file %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("codex hooks path %q is not a regular file", path)
	}
	if opened.Size() > maxHooksFileBytes {
		return nil, fmt.Errorf("codex hooks file %q exceeds %d bytes", path, maxHooksFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex hooks file %q: %w", path, err)
	}
	if len(data) > maxHooksFileBytes {
		return nil, fmt.Errorf("codex hooks file %q exceeds %d bytes", path, maxHooksFileBytes)
	}
	document.exists = true
	if err := json.Unmarshal(data, &document.topLevel); err != nil {
		return nil, fmt.Errorf("failed to parse existing hooks.json %q: %w", path, err)
	}
	if document.topLevel == nil {
		document.topLevel = make(map[string]json.RawMessage)
	}
	if hooksRaw, ok := document.topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &document.rawHooks); err != nil {
			return nil, fmt.Errorf("failed to parse hooks in hooks.json %q: %w", path, err)
		}
	}
	if document.rawHooks == nil {
		document.rawHooks = make(map[string]json.RawMessage)
	}
	return document, nil
}

func readHooksFileForMutation(cfg *agent.HookConfigFile) ([]byte, bool, error) {
	// Bounded before the read rather than measured after it, matching
	// readHooksDocument above. cfg.Read is an unbounded io.ReadAll, and
	// .codex/hooks.json arrives with the checkout, so a large committed file
	// was read into memory in full before the size was ever checked.
	root, name := cfg.Root()
	file, err := osroot.OpenNoFollow(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open Codex hooks file %q: %w", cfg.Path(), err)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect opened Codex hooks file %q: %w", cfg.Path(), err)
	}
	if !opened.Mode().IsRegular() {
		return nil, false, fmt.Errorf("codex hooks path %q is not a regular file", cfg.Path())
	}
	if opened.Size() > maxHooksFileBytes {
		return nil, false, fmt.Errorf("codex hooks file %q exceeds %d bytes", cfg.Path(), maxHooksFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read Codex hooks file %q: %w", cfg.Path(), err)
	}
	if len(data) > maxHooksFileBytes {
		return nil, false, fmt.Errorf("codex hooks file %q exceeds %d bytes", cfg.Path(), maxHooksFileBytes)
	}
	return data, true, nil
}

func hookFileStateForReadError(err error) HookFileState {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return HookFileMalformed
	}
	if strings.Contains(err.Error(), "failed to parse") {
		return HookFileMalformed
	}
	return HookFileUnavailable
}

// managedHookIsCurrent reports whether the event has exactly one Entire hook,
// it is want, and it is in an unfiltered group, which is the state
// syncHookCommand leaves; anything else is what `entire enable` would rewrite.
func managedHookIsCurrent(groups []MatcherGroup, want HookEntry) bool {
	count := 0
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if !isEntireHook(hook.Command) {
				continue
			}
			if group.Matcher != nil || !sameManagedSettings(hook, want) {
				return false
			}
			count++
		}
	}
	return count == 1
}

// sameManagedSettings reports whether hook is want: both commands and the
// timeout. The timeout is part of the match so an upgrade rewrites a hook
// installed by an older Entire with a different budget — notably SessionEnd,
// where a leftover 30s makes Codex print a clamping warning at every startup.
// commandWindows is part of it so a hook written before Entire set one, or
// with a Windows command from an older release, is replaced rather than left
// to run the wrong command on Windows.
func sameManagedSettings(hook, want HookEntry) bool {
	return hook.Command == want.Command && hook.CommandWindows == want.CommandWindows && hook.Timeout == want.Timeout
}

// --- Helpers ---

func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]MatcherGroup) error {
	if data, ok := rawHooks[hookType]; ok {
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("failed to parse %s hooks: %w", hookType, err)
		}
	}
	return nil
}

func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, groups []MatcherGroup) {
	if len(groups) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(groups)
	if err != nil {
		return
	}
	rawHooks[hookType] = data
}

// hookEntryExists reports whether want is already configured exactly.
func hookEntryExists(groups []MatcherGroup, want HookEntry) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if sameManagedSettings(hook, want) {
				return true
			}
		}
	}
	return false
}

// syncHookCommand makes want the event's one Entire hook. The Entire hook
// already there (an exact match first, else the first one an older release
// wrote) is updated in place, so fields the user added to it, such as
// statusMessage or async, survive the upgrade. If it sits in a group with a
// matcher, current or not, it moves, fields and all, to the unfiltered group
// Entire installs into, so it fires for every tool again; the group's own
// fields stay with the group and its remaining hooks, and a group left with no
// hooks is dropped. Any other Entire hook for the event is removed, and want is
// added when there is none. It applies
// agent.DropStaleManagedHooks' rule (every install drops Entire hooks an older
// release left behind) through the same IsManagedHookCommand recognition, but
// rewrites the one it keeps instead of re-adding it.
func syncHookCommand(groups []MatcherGroup, want HookEntry) ([]MatcherGroup, bool) {
	keep := func(hook HookEntry) bool { return sameManagedSettings(hook, want) }
	if !hookEntryExists(groups, want) {
		keep = func(hook HookEntry) bool { return isEntireHook(hook.Command) }
	}
	kept, changed := false, false
	var moved *HookEntry
	result := make([]MatcherGroup, 0, len(groups))
	for _, group := range groups {
		hooks := make([]HookEntry, 0, len(group.Hooks))
		for _, hook := range group.Hooks {
			switch {
			case !kept && keep(hook):
				kept = true
				if !sameManagedSettings(hook, want) || hook.Type != want.Type {
					hook = hook.withManagedSettings(want)
					changed = true
				}
				if group.Matcher != nil {
					moved = &hook
					continue
				}
			case isEntireHook(hook.Command):
				changed = true
				continue
			}
			hooks = append(hooks, hook)
		}
		if len(hooks) > 0 {
			group.Hooks = hooks
			result = append(result, group)
		}
	}
	switch {
	case !kept:
		return addHook(result, want), true
	case moved != nil:
		return addHook(result, *moved), true
	case !changed:
		return groups, false
	}
	return result, true
}

func addHook(groups []MatcherGroup, entry HookEntry) []MatcherGroup {
	// Add to an existing group with null matcher, or create a new one
	for i, group := range groups {
		if group.Matcher == nil {
			groups[i].Hooks = append(groups[i].Hooks, entry)
			return groups
		}
	}
	return append(groups, MatcherGroup{
		Matcher: nil,
		Hooks:   []HookEntry{entry},
	})
}

func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

func hasEntireHook(groups []MatcherGroup) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func removeEntireHooks(groups []MatcherGroup) []MatcherGroup {
	result := make([]MatcherGroup, 0, len(groups))
	for _, group := range groups {
		filtered := make([]HookEntry, 0, len(group.Hooks))
		for _, hook := range group.Hooks {
			if !isEntireHook(hook.Command) {
				filtered = append(filtered, hook)
			}
		}
		if len(filtered) > 0 {
			group.Hooks = filtered
			result = append(result, group)
		}
	}
	return result
}

// HookConfigRelPath implements agent.HookConfigLocator.
func (c *CodexAgent) HookConfigRelPath() string { return ".codex/" + HooksFileName }

// EntireHookCommands returns, by event, every command Entire installs or has
// installed for that event: today's `command` and `commandWindows`, plus the
// Windows wrapper older releases wrote into `command` on hosts where sh did not
// run. Callers must match whole commands, and must check a hook's Windows
// command as well as its `command`, since Codex on Windows runs the former.
func EntireHookCommands() map[string][]string {
	const cmdPrefix = "entire hooks codex "
	out := make(map[string][]string)
	for _, hook := range managedHooks {
		posix, windows := hook.commands()
		out[hook.event] = append(out[hook.event], posix, windows)
		if legacy := hook.legacyWindows(cmdPrefix + hook.verb); legacy != windows {
			out[hook.event] = append(out[hook.event], legacy)
		}
	}
	return out
}

// WindowsCommandKeys returns the hook fields Codex runs in place of `command`
// on Windows: commandWindows, and the command_windows spelling it also accepts.
func WindowsCommandKeys() []string {
	return []string{"commandWindows", "command_windows"}
}
