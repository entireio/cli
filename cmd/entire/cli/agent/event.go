package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
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

	// ToolInput is the raw tool input JSON (for subagent type/description extraction).
	// Used when both SubagentType and TaskDescription are empty (agents that don't provide
	// these fields directly parse them from ToolInput).
	ToolInput json.RawMessage

	// SubagentType is the kind of subagent (for SubagentStart/SubagentEnd events).
	// Used with TaskDescription instead of ToolInput
	SubagentType    string
	TaskDescription string

	// ModifiedFiles is a list of file paths modified by a subagent.
	// Populated on SubagentEnd events when the agent provides this data
	// directly via hook payload (e.g., Cursor's subagentStop).
	ModifiedFiles []string

	// ResponseMessage is an optional message to display to the user via the agent.
	ResponseMessage string

	// Hook-provided session metrics (populated by agents that report these via hooks).
	DurationMs        int64 // Session duration from agent hook (e.g., Cursor SessionEnd)
	TurnCount         int   // Number of agent turns/loops (e.g., Cursor Stop hook)
	ContextTokens     int   // Context window tokens used (e.g., Cursor PreCompact hook)
	ContextWindowSize int   // Total context window size (e.g., Cursor PreCompact hook)

	// Metadata holds agent-specific state that the framework stores and makes available
	// on subsequent events. Examples: Pi's activeLeafId, Cursor's is_background_agent.
	Metadata map[string]string
}

// ReadAndParseHookInput reads all bytes from stdin and unmarshals JSON into the given type.
// This is a shared helper for agent ParseHookEvent implementations.
func ReadAndParseHookInput[T any](stdin io.Reader) (*T, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to read hook input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty hook input")
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	return &result, nil
}
