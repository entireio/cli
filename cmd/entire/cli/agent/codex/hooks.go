package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
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

// managedHook adds the platform-specific command wrapper to one canonical
// event specification.
type managedHook struct {
	event   string // hooks.json key
	label   string // Codex trust-state key
	verb    string // `entire hooks codex <verb>`
	timeout int
	wrap    func(cmd string, windows bool) string

	// core marks the events whose absence means Codex was never enabled in this
	// repo, as opposed to enabled against an older release that installed fewer
	// events. Only these gate AreHooksInstalled — see the comment there.
	core bool
}

func buildManagedHooks() []managedHook {
	managed := make([]managedHook, 0, len(hookEventSpecs))
	for _, spec := range hookEventSpecs {
		if !spec.Managed {
			continue
		}
		wrap := agent.WrapProductionSilentHookCommandForOS
		if spec.JSONWarning {
			wrap = func(cmd string, windows bool) string {
				return agent.WrapProductionJSONWarningHookCommandForOS(cmd, agent.WarningFormatSingleLine, windows)
			}
		}
		managed = append(managed, managedHook{
			event: spec.Event, label: spec.Label, verb: spec.Verb,
			timeout: spec.Timeout, core: spec.Core, wrap: wrap,
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
	hooksPath := worktreeHooks.Path()
	existingData, exists, err := readHooksFileForMutation(hooksPath)
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

	const cmdPrefix = "entire hooks codex "
	useWindowsProductionHooks := agent.UseWindowsProductionHooks(ctx)
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
		hookCmd := h.wrap(cmdPrefix+h.verb, useWindowsProductionHooks)
		if synced, changed := syncHookCommand(groups, hookCmd, h.timeout); changed {
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
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .codex directory: %w", err)
	}
	if err := validateMutableHookTarget(worktreeHooks); err != nil {
		return 0, err
	}
	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write hooks.json: %w", err)
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
	hooksPath := worktreeHooks.Path()
	data, exists, err := readHooksFileForMutation(hooksPath)
	if err != nil {
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
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write hooks.json: %w", err)
	}
	return nil
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
func (c *CodexAgent) AreHooksInstalled(ctx context.Context) bool {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return false
	}
	if err := validateMutableHookTarget(worktreeHooks); err != nil {
		return false
	}
	data, exists, err := readHooksFileForMutation(worktreeHooks.Path())
	if err != nil || !exists {
		return false
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return false
	}
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return false
		}
	}
	for _, h := range managedHooks {
		if !h.core {
			continue
		}
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, h.event, &groups); err != nil || !hasEntireHook(groups) {
			return false
		}
	}
	return true
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
	event   string
	label   string
	command string
	timeout int
	core    bool
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

func managedHookSpecs(ctx context.Context) []managedHookSpec {
	const cmdPrefix = "entire hooks codex "
	useWindowsProductionHooks := agent.UseWindowsProductionHooks(ctx)
	specs := make([]managedHookSpec, 0, len(managedHooks))
	for _, hook := range managedHooks {
		command := hook.wrap(cmdPrefix+hook.verb, useWindowsProductionHooks)
		specs = append(specs, managedHookSpec{
			event:   hook.event,
			label:   hook.label,
			command: command,
			timeout: hook.timeout,
			core:    hook.core,
		})
	}
	return specs
}

// InspectHookConfig resolves and parses the hooks file Codex discovers.
func InspectHookConfig(ctx context.Context) HookConfigInspection {
	discovery := ResolveHookDiscovery(ctx)
	if discovery.State != HookDiscoveryResolved {
		return HookConfigInspection{State: HookFileUnavailable, Err: discovery.Diagnostic}
	}
	return inspectDiscoveredHookConfig(ctx, discovery.DiscoveredHooks)
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

func inspectHookConfigAt(ctx context.Context, path string) HookConfigInspection {
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
	for _, spec := range managedHookSpecs(ctx) {
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
		if !managedHookIsCurrent(groups, spec.command, spec.timeout) {
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

func readHooksDocument(path string) (*hooksDocument, error) {
	document := &hooksDocument{
		topLevel: make(map[string]json.RawMessage),
		rawHooks: make(map[string]json.RawMessage),
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Codex project directory for %q: %w", path, err)
	}
	defer root.Close()

	name := filepath.Base(path)
	before, err := root.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect Codex hooks file %q: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("codex hooks path %q is not a regular file", path)
	}
	if before.Size() > maxHooksFileBytes {
		return nil, fmt.Errorf("codex hooks file %q exceeds %d bytes", path, maxHooksFileBytes)
	}

	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open Codex hooks file %q: %w", path, err)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened Codex hooks file %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("codex hooks file %q changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex hooks file %q: %w", path, err)
	}
	if len(data) > maxHooksFileBytes {
		return nil, fmt.Errorf("codex hooks file %q exceeds %d bytes", path, maxHooksFileBytes)
	}
	after, err := root.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("reinspect Codex hooks file %q: %w", path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("codex hooks file %q changed while reading", path)
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

func readHooksFileForMutation(path string) ([]byte, bool, error) {
	file, err := os.Open(path) //nolint:gosec // path is validated against the current worktree
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Codex hooks file %q: %w", path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read Codex hooks file %q: %w", path, err)
	}
	if len(data) > maxHooksFileBytes {
		return nil, false, fmt.Errorf("codex hooks file %q exceeds %d bytes", path, maxHooksFileBytes)
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

func managedHookIsCurrent(groups []MatcherGroup, command string, timeoutSec int) bool {
	count := 0
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if !isEntireHook(hook.Command) {
				continue
			}
			if hook.Command != command || hook.Timeout != timeoutSec {
				return false
			}
			count++
		}
	}
	return count == 1
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

// hookCommandExists reports whether the exact command is already configured
// with the timeout we want. The timeout is part of the match so an upgrade
// rewrites a hook installed by an older Entire with a different budget —
// notably SessionEnd, where a leftover 30s makes Codex print a clamping warning
// at every startup.
func hookCommandExists(groups []MatcherGroup, command string, timeoutSec int) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if hook.Command == command && hook.Timeout == timeoutSec {
				return true
			}
		}
	}
	return false
}

// syncHookCommand ensures groups contains exactly the requested Entire hook and
// removes stale commands left by an older install.
func syncHookCommand(groups []MatcherGroup, command string, timeoutSec int) ([]MatcherGroup, bool) {
	groups, dropped := dropStaleEntireHooks(groups, command, timeoutSec)
	if hookCommandExists(groups, command, timeoutSec) {
		return groups, dropped
	}
	return addHook(groups, command, timeoutSec), true
}

func dropStaleEntireHooks(groups []MatcherGroup, command string, timeoutSec int) ([]MatcherGroup, bool) {
	staleTimeout := func(e HookEntry) bool { return e.Command == command && e.Timeout != timeoutSec }
	result := make([]MatcherGroup, 0, len(groups))
	dropped := false
	for _, group := range groups {
		kept, removed := agent.DropStaleManagedHooks(group.Hooks, hookEntryCommand, []string{command})
		dropped = dropped || removed
		if slices.ContainsFunc(kept, staleTimeout) {
			kept = slices.DeleteFunc(slices.Clone(kept), staleTimeout)
			dropped = true
		}
		if len(kept) > 0 {
			group.Hooks = kept
			result = append(result, group)
		}
	}
	if !dropped {
		return groups, false
	}
	return result, true
}

func hookEntryCommand(e HookEntry) string { return e.Command }

func addHook(groups []MatcherGroup, command string, timeoutSec int) []MatcherGroup {
	entry := HookEntry{
		Type:    "command",
		Command: command,
		Timeout: timeoutSec,
	}

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
