package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// EventType represents a normalized lifecycle event from any agent.
// Agents translate their native hooks into these event types via ParseHookEvent.
type EventType int

const (
	// SessionStart indicates the agent session has begun.
	SessionStart EventType = iota + 1

	// TurnStart indicates the user submitted a prompt and the agent is about to work.
	TurnStart

	// TurnEnd indicates the agent finished responding to a prompt.
	TurnEnd

	// Compaction indicates the agent is about to compress its context window.
	// This triggers the same save logic as TurnEnd but also resets the transcript offset.
	Compaction

	// SessionEnd indicates the session has been terminated.
	SessionEnd

	// SubagentStart indicates a subagent (task) has been spawned.
	SubagentStart

	// SubagentEnd indicates a subagent (task) has completed.
	SubagentEnd

	// ModelUpdate indicates the agent reported the LLM model being used.
	// This fires on hooks that carry model info but have no other lifecycle action
	// (e.g., Gemini CLI's BeforeModel). The framework stores the model as a hint
	// for subsequent TurnStart/TurnEnd events in the same session.
	ModelUpdate

	// ToolUse indicates the agent ran a tool that touched files mid-turn.
	// Carries ModifiedFiles/NewFiles/DeletedFiles so the framework can populate
	// state.FilesTouched incrementally — without this, agents like Codex that
	// commit mid-turn (before TurnEnd fires) have no per-tool file accounting,
	// and the carry-forward path falls back to whole-transcript extraction.
	ToolUse
)

// String returns a human-readable name for the event type.
func (e EventType) String() string {
	switch e {
	case SessionStart:
		return "SessionStart"
	case TurnStart:
		return "TurnStart"
	case TurnEnd:
		return "TurnEnd"
	case Compaction:
		return "Compaction"
	case SessionEnd:
		return "SessionEnd"
	case SubagentStart:
		return "SubagentStart"
	case SubagentEnd:
		return "SubagentEnd"
	case ModelUpdate:
		return "ModelUpdate"
	case ToolUse:
		return "ToolUse"
	default:
		return "Unknown"
	}
}

// Event is a normalized lifecycle event produced by an agent's ParseHookEvent method.
// The framework dispatcher uses these events to drive checkpoint/session lifecycle actions.
type Event struct {
	// Type is the kind of lifecycle event.
	Type EventType

	// SessionID identifies the agent session.
	SessionID string

	// PreviousSessionID is non-empty when this event represents a session continuation
	// or handoff (e.g., Claude starting a new session ID after exiting plan mode).
	PreviousSessionID string

	// SessionRef is an agent-specific reference to the transcript (typically a file path).
	SessionRef string

	// Prompt is the user's prompt text (populated on TurnStart events).
	Prompt string

	// Model is the LLM model identifier (e.g., "claude-sonnet-4-20250514").
	// Populated on SessionStart (Claude Code), ModelUpdate (Gemini CLI BeforeModel),
	// and TurnStart/TurnEnd events when the agent provides model info.
	Model string

	// Timestamp is when the event occurred.
	Timestamp time.Time

	// ToolUseID identifies the tool invocation (for SubagentStart/SubagentEnd events).
	ToolUseID string

	// SubagentID identifies the subagent instance (for SubagentEnd events).
	SubagentID string

	// Final is true only for events that represent true completion of a
	// subagent (Claude Code's SubagentStop), never for the launch-time
	// PostToolUse SubagentEnd, which fires at the background launch stub
	// seconds after launch. Downstream lifecycle branching keys off this flag,
	// not any payload sentinel. Final is the disambiguator for agents with a
	// two-signal model (a launch-time stub plus a separate completion hook,
	// like Claude Code's background tasks); agents whose single subagent-end
	// event already fires at true completion must leave it false so the
	// existing pipeline handles them unchanged.
	Final bool

	// SubagentTranscriptPath is the agent-declared path to the subagent's own
	// transcript (SubagentEnd). Set it whenever the hook payload names the file;
	// Codex and Cursor both send agent_transcript_path, as does Claude Code's
	// SubagentStop.
	//
	// When empty the framework probes the layout Claude Code and Factory AI Droid
	// share (cli.ResolveAgentTranscriptPath). For any other agent that probe finds
	// nothing and yields "" silently, so the only symptom is a task checkpoint with
	// no subagent transcript plus file extraction falling back to the main
	// transcript — where a subagent's edits never appear. Declaring the path is how
	// an agent opts out of that guess.
	SubagentTranscriptPath string

	// ToolInput is the raw tool input JSON (for subagent type/description extraction).
	// Used when both SubagentType and TaskDescription are empty (agents that don't provide
	// these fields directly parse them from ToolInput).
	ToolInput json.RawMessage

	// SubagentType is the kind of subagent (for SubagentStart/SubagentEnd events).
	// Used with TaskDescription instead of ToolInput
	SubagentType    string
	TaskDescription string

	// ModifiedFiles is the list of file paths modified by a subagent (SubagentEnd)
	// or a tool call (ToolUse). Paths may be absolute, cwd-relative, or
	// repo-relative; lifecycle handlers normalize against the worktree root.
	ModifiedFiles []string

	// NewFiles and DeletedFiles carry create/delete paths for ToolUse events,
	// kept separate from ModifiedFiles so consumers can reason about agent intent.
	NewFiles     []string
	DeletedFiles []string

	// CWD is the working directory the agent was running in when the event fired.
	// Set on ToolUse so cwd-relative payload paths can be resolved before
	// repo-root normalization.
	CWD string

	// ResponseMessage is an optional message to display to the user via the agent.
	ResponseMessage string

	// Hook-provided session metrics (populated by agents that report these via hooks).
	DurationMs        int64 // Session duration from agent hook (e.g., Cursor SessionEnd)
	TurnCount         int   // Number of agent turns/loops (e.g., Cursor Stop hook)
	ContextTokens     int   // Context window tokens used (e.g., Cursor PreCompact hook)
	ContextWindowSize int   // Total context window size (e.g., Cursor PreCompact hook)

	// TokenUsage carries per-turn token accounting reported by an agent hook
	// directly (e.g., Cursor's Stop hook). Set when the hook payload contains
	// authoritative token data that the JSONL transcript does not. Lifecycle
	// handlers prefer this over transcript-based calculation when populated.
	TokenUsage *TokenUsage

	// SkillEvents records native agent skill signals surfaced by hooks.
	// The lifecycle layer persists these to session state and later checkpoint metadata.
	SkillEvents []SkillEvent

	// Metadata holds agent-specific state that the framework stores and makes available
	// on subsequent events. Examples: Pi's activeLeafId, Cursor's is_background_agent.
	Metadata map[string]string
}

// ReadAndParseHookInput decodes a single JSON hook payload from stdin into the
// given type. This is a shared helper for agent ParseHookEvent implementations.
//
// It deliberately does NOT use io.ReadAll, which waits for stdin to reach EOF.
// Agents drive hooks by piping a JSON payload to the hook process, but some
// keep the write end of that pipe open for the hook's lifetime rather than
// closing it after writing — notably on Windows/Git Bash, where a full payload
// arrives but EOF never does. io.ReadAll then blocked indefinitely and the hook
// (e.g. gemini session-start) hung forever (issue #1398). A streaming
// json.Decoder returns as soon as one complete JSON value has been read,
// independent of when — or whether — stdin is closed.
func ReadAndParseHookInput[T any](stdin io.Reader) (*T, error) {
	raw, err := ReadHookInputRaw(stdin)
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	return &result, nil
}

// ReadHookInputRaw returns the raw bytes of a single JSON hook payload read from
// stdin, without waiting for EOF. It is the shared primitive behind every
// agent's hook-input read (issue #1398); callers that need custom parsing
// (e.g. key-name fallbacks, or forwarding the bytes to a subprocess) use this
// directly, while the common case uses ReadAndParseHookInput.
func ReadHookInputRaw(stdin io.Reader) (json.RawMessage, error) {
	return ReadHookInputRawLimited(stdin, -1)
}

// ReadHookInputRawLimited is ReadHookInputRaw with a ceiling of limit bytes on
// the payload read from stdin, a leading BOM included (limit < 0 means
// unlimited). It is used at the external/plugin boundary to bound an untrusted
// payload — without reintroducing the EOF-wait hang, since the streaming
// decoder still returns on the first complete value.
//
// The ceiling is the outermost wrapper, so the buffered reader skipUTF8BOM adds
// fills through it: its read-ahead is then bounded by limit rather than by its
// own buffer size, which is larger than plausible ceilings.
func ReadHookInputRawLimited(stdin io.Reader, limit int64) (json.RawMessage, error) {
	// If stdin is an interactive terminal there is no payload coming at all: the
	// command was run by hand, or the agent left the console attached instead of
	// wiring up a pipe. Decoding would block waiting for input that never comes,
	// so treat it as empty and return promptly.
	if StdinLooksInteractive(stdin) {
		return nil, errors.New("empty hook input")
	}

	r := stdin
	if limit >= 0 {
		r = io.LimitReader(stdin, limit)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(skipUTF8BOM(r)).Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty hook input")
		}
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	return raw, nil
}

// StdinLooksInteractive reports whether r is an interactive terminal, i.e. no
// piped hook payload is on its way. Hook readers use it to bail out promptly
// instead of blocking on a read that will never complete (issue #1398).
func StdinLooksInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) //nolint:gosec // G115: uintptr->int is safe for fd
}

// utf8BOM is the UTF-8 serialization of U+FEFF. JSON may not begin with it and
// Go's decoder does not skip it, so a payload carrying one fails to parse as
// `invalid character 'ï' looking for beginning of value`.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// skipUTF8BOM returns a reader positioned past any UTF-8 BOMs at the head of r.
//
// A hook payload acquires one when the agent pipes it through Windows
// PowerShell. Cursor runs every hook command as
// `$OutputEncoding = [System.Text.Encoding]::UTF8; … | & { $input | <command> }`,
// and under Windows PowerShell 5.1 that encoder carries a 3-byte preamble which
// is written ahead of the JSON — the default $OutputEncoding there is ASCII,
// which is why Cursor sets it at all. Measured on Windows 11 / PowerShell 5.1:
// that line alone adds one BOM at any console codepage, and a UTF-8 console
// codepage adds a second independently, so both stack. Hence a loop rather than
// one strip. It terminates when the input does, and the untrusted boundary
// bounds the input with ReadHookInputRawLimited's ceiling.
//
// Each strip is gated on a ONE-byte peek. Peek(3) blocks until three bytes
// arrive or the reader fails, so peeking three unconditionally would give a
// runner that writes a short-but-complete payload and holds the pipe open the
// very hang this reader exists to avoid (issue #1398). No JSON document legally
// begins with 0xEF, so only input that is already malformed reaches the wider
// peek.
//
// The gate bounds that hang to valid payloads, and does not abolish it: input
// that is nothing but a BOM used to fail instantly on its first byte, and now
// leaves the decoder with nothing to read until the writer closes or the host
// kills the hook. That is the price of stripping at all — a single strip pays it
// too — and it is only ever paid by input that was already malformed.
//
// Every exit is the same one, and reader errors are deliberately not
// propagated: an empty or failing reader is handed to the decoder untouched, so
// it still produces the io.EOF that callers report as "empty hook input" rather
// than a bufio-shaped error in its place.
func skipUTF8BOM(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	for {
		first, err := br.Peek(1)
		if err != nil || first[0] != utf8BOM[0] {
			return br
		}
		head, err := br.Peek(len(utf8BOM))
		if err != nil || !bytes.Equal(head, utf8BOM) {
			return br
		}
		if _, err := br.Discard(len(utf8BOM)); err != nil {
			return br
		}
	}
}
