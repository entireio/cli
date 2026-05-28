package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Antigravity hook name constants — these become subcommands under `entire hooks antigravity`.
const (
	HookNamePreToolUse     = "pre-tool-use"
	HookNamePostToolUse    = "post-tool-use"
	HookNamePreInvocation  = "pre-invocation"
	HookNamePostInvocation = "post-invocation"
	HookNameStop           = "stop"
)

// HookNames returns the hook verbs Antigravity supports.
// These become subcommands: entire hooks antigravity <verb>
func (a *AntigravityAgent) HookNames() []string {
	return []string{
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNamePreInvocation,
		HookNamePostInvocation,
		HookNameStop,
	}
}

// ParseHookEvent translates an Antigravity hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance (e.g., post-tool-use).
func (a *AntigravityAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNamePreInvocation:
		return parsePreInvocation(stdin)
	case HookNamePostInvocation:
		return parsePostInvocation(stdin)
	case HookNameStop:
		return parseStop(stdin)
	case HookNamePreToolUse:
		return parsePreToolUse(stdin)
	case HookNamePostToolUse:
		// PostToolUse has no lifecycle significance in v1
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// parsePreInvocation handles the PreInvocation hook.
//
// Emits TurnStart ONLY on the first model invocation of a conversation
// (invocationNum == 0). Subsequent PreInvocations within the same
// conversation return nil.
//
// Background: agy's PreInvocation fires per *model invocation*, but Entire's
// TurnStart event is designed for per-*user-prompt*. The framework's TurnStart
// handler re-captures pre-prompt state (preUntrackedFiles, attribution
// baseline) on every call. If we emit TurnStart on every PreInvocation, the
// baseline gets clobbered each time — by the time TurnEnd fires at Stop, the
// pre-state reflects the post-tool-use snapshot, and DetectFileChanges sees
// no new files compared to itself ("no files modified during session,
// skipping checkpoint"). Confirmed by agy traces showing two PreInvocations
// per single-prompt conversation.
//
// agy wire-format quirk: invocationNum is **0-indexed** despite the docs
// describing it as "the sequence number of the current model invocation"
// (which most CLI tools interpret as 1-based). Real captured stdin from
// agy 1.0.0:
//
//	PreInvocation #1: {"invocationNum":0,"initialNumSteps":1,...}  ← turn start
//	PreInvocation #2: {"invocationNum":1,"initialNumSteps":5,...}  ← follow-up
//
// (initialNumSteps is not a usable "first?" signal — agy inserts the user
// prompt as a step before the first model call, so it's already 1.)
//
// Limitation: agy resumes (agy --continue / --conversation) start with
// invocationNum > 0, so they won't fire TurnStart. If the prior session state
// was already cleaned up (FullyCondensed), the resumed turn won't be tracked
// until the user starts a fresh conversation. Tracked in deferred work.
//
// Antigravity has no SessionStart hook surface, so there is no path to display
// a "tracked by entire" banner in the agy UI for v1. AntigravityAgent
// intentionally does not implement HookResponseWriter, matching the
// Cursor/OpenCode/Copilot/Pi pattern of silent session tracking.
func parsePreInvocation(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[InvocationPayload](stdin)
	if err != nil {
		return nil, err
	}
	if raw.InvocationNum != 0 {
		return nil, nil //nolint:nilnil // follow-up model invocation, not a new turn
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.ConversationID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

// parsePostInvocation handles the PostInvocation hook.
//
// Returning nil instead of a TurnEnd event is deliberate: Antigravity writes
// its transcript file (~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript.jsonl)
// AFTER the Stop hook fires, not before PostInvocation. Emitting TurnEnd here
// would route the event through handleLifecycleTurnEnd, which requires the
// transcript file to exist (cli/lifecycle.go fileExists check) and would
// return exit 1 — terminating agy's agent turn.
//
// SessionEnd processing on Stop (with fullyIdle=true) already handles the
// session-finalization work safely, so PostInvocation as a no-op loses
// nothing material in v1.
func parsePostInvocation(stdin io.Reader) (*agent.Event, error) {
	// Decode and discard — we still validate the payload shape, just don't
	// surface a lifecycle event.
	if _, err := agent.ReadAndParseHookInput[InvocationPayload](stdin); err != nil {
		return nil, err
	}
	return nil, nil //nolint:nilnil // PostInvocation has no lifecycle action in v1 (see comment above)
}

// parseStop handles the Stop hook.
//
// Returns TurnEnd when fullyIdle=true; returns nil when background tasks are
// still running (fullyIdle=false) so the session isn't finalized prematurely.
//
// We map fullyIdle=true to TurnEnd (not SessionEnd) because the framework's
// TurnEnd handler invokes SaveStep — which increments StepCount, writes a
// checkpoint to the shadow branch, and persists FilesTouched into the per-
// session metadata. Without that, the eventual `git commit` finds no shadow
// branch for the session and the cleanup pass at listAllSessionStates removes
// the state file before any checkpoint is condensed. Mapping to SessionEnd
// would mark the session ENDED but never run SaveStep, leaving files_touched
// in a state that never produces a checkpoint commit.
//
// Antigravity's lifecycle gives us exactly one definite "model loop finished"
// moment (Stop with fullyIdle=true), so it's the right anchor for TurnEnd.
// Multi-turn agy sessions get a single TurnEnd at exit, capturing the entire
// turn's work in one checkpoint — a deliberate trade-off vs the per-prompt
// granularity other agents (Gemini, Claude) achieve via separate BeforeAgent
// /AfterAgent or UserPromptSubmit/Stop hooks. See PrepareTranscript below for
// the asynchronous-transcript handling.
func parseStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[StopPayload](stdin)
	if err != nil {
		return nil, err
	}
	if !raw.FullyIdle {
		return nil, nil //nolint:nilnil // Background tasks running — do not end session yet
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.ConversationID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

// parsePreToolUse handles the PreToolUse hook → ToolUse for mutating tools.
// Returns nil for non-mutating tools (no lifecycle action needed).
func parsePreToolUse(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[PreToolUsePayload](stdin)
	if err != nil {
		return nil, err
	}
	modifiedFiles, newFiles := extractFilesFromToolCall(&raw.ToolCall)
	if modifiedFiles == nil && newFiles == nil {
		return nil, nil //nolint:nilnil // Non-mutating tool — no lifecycle action
	}
	return &agent.Event{
		Type:          agent.ToolUse,
		SessionID:     raw.ConversationID,
		SessionRef:    raw.TranscriptPath,
		ModifiedFiles: modifiedFiles,
		NewFiles:      newFiles,
		Timestamp:     time.Now(),
	}, nil
}

// resolveAgySymlinks resolves symlinks in the parent directory of an absolute
// path so paths agy sends (e.g. /tmp/foo/bar.md on macOS) match the symlink-
// resolved worktree root the framework uses (/private/tmp/foo). Without this,
// the framework's FilterAndNormalizePaths produces a "../" relative path and
// drops the file as "outside repo" — silently breaking files_touched capture.
//
// We resolve the PARENT directory only, not the file itself, because during
// PreToolUse the file may not exist yet (write_to_file is creating it).
// Returns the input unchanged if it's not absolute or symlink resolution fails.
func resolveAgySymlinks(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	parent := filepath.Dir(p)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return p
	}
	return filepath.Join(resolved, filepath.Base(p))
}

// extractFilesFromToolCall inspects the tool call and returns the files it
// will modify or create. Both slices are nil for non-mutating tools.
//
// agy 1.0.0 wire-format quirk: every tool arg value is double-encoded as a
// JSON string containing the actual value. So instead of:
//
//	{"TargetFile": "/path/to/file", "Overwrite": true}
//
// the hook actually receives:
//
//	{"TargetFile": "\"/path/to/file\"", "Overwrite": "true"}
//
// This is undocumented but consistent. To stay robust against both the
// docs-shape format and the actual agy 1.0.0 format, we parse args into raw
// values and unquote/coerce on the way out.
func extractFilesFromToolCall(tc *ToolCall) (modifiedFiles, newFiles []string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(tc.Args, &raw); err != nil {
		return nil, nil
	}

	switch tc.Name {
	case "write_to_file":
		targetFile := resolveAgySymlinks(decodeAgyString(raw["TargetFile"]))
		if targetFile == "" {
			return nil, nil
		}
		if decodeAgyBool(raw["Overwrite"]) {
			return []string{targetFile}, nil
		}
		return nil, []string{targetFile}

	case "replace_file_content", "multi_replace_file_content":
		targetFile := resolveAgySymlinks(decodeAgyString(raw["TargetFile"]))
		if targetFile == "" {
			return nil, nil
		}
		return []string{targetFile}, nil

	default:
		return nil, nil
	}
}

// decodeAgyString handles agy's double-encoded string args. Tries the
// docs-shape format first (a plain JSON string), then falls back to the
// agy-actual format (a JSON string whose content is itself a JSON-encoded
// string). Returns "" when neither form decodes cleanly.
func decodeAgyString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	// agy double-encodes — unwrap once more if the inner content is itself JSON-quoted.
	var inner string
	if err := json.Unmarshal([]byte(s), &inner); err == nil {
		return inner
	}
	return s
}

// decodeAgyBool handles agy's double-encoded bool args. Tries the docs-shape
// format (real JSON boolean) first, then the agy-actual format (string "true"
// or "false"). Returns false for any unrecognized shape.
func decodeAgyBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == "true"
	}
	return false
}
