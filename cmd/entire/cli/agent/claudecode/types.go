package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// ClaudeSettings represents the .claude/settings.json structure
type ClaudeSettings struct {
	Hooks ClaudeHooks `json:"hooks"`
}

// ClaudeHooks contains the hook configurations
type ClaudeHooks struct {
	SessionStart     []ClaudeHookMatcher `json:"SessionStart,omitempty"`
	SessionEnd       []ClaudeHookMatcher `json:"SessionEnd,omitempty"`
	UserPromptSubmit []ClaudeHookMatcher `json:"UserPromptSubmit,omitempty"`
	Stop             []ClaudeHookMatcher `json:"Stop,omitempty"`
	StopFailure      []ClaudeHookMatcher `json:"StopFailure,omitempty"`
	SubagentStart    []ClaudeHookMatcher `json:"SubagentStart,omitempty"`
	SubagentStop     []ClaudeHookMatcher `json:"SubagentStop,omitempty"`
	PreToolUse       []ClaudeHookMatcher `json:"PreToolUse,omitempty"`
	PostToolUse      []ClaudeHookMatcher `json:"PostToolUse,omitempty"`
}

// ClaudeHookMatcher matches hooks to specific patterns
type ClaudeHookMatcher struct {
	Matcher string            `json:"matcher"`
	Hooks   []ClaudeHookEntry `json:"hooks"`
	extra   map[string]json.RawMessage
}

// claudeHookMatcherFields is ClaudeHookMatcher without its JSON methods.
type claudeHookMatcherFields ClaudeHookMatcher

func (m *ClaudeHookMatcher) UnmarshalJSON(data []byte) error {
	*m = ClaudeHookMatcher{} // see ClaudeHookEntry.UnmarshalJSON
	extra, _, err := unmarshalKeepingExtra(data, (*claudeHookMatcherFields)(m), "matcher", "hooks")
	m.extra = extra
	return err
}

func (m ClaudeHookMatcher) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(claudeHookMatcherFields(m), m.extra)
}

// ClaudeHookEntry represents a single hook command
type ClaudeHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	// Timeout is the hook's timeout in seconds. Omitted (0) leaves Claude Code's
	// default in place; set only where a hook needs an explicit budget.
	Timeout int `json:"timeout,omitempty"`
	extra   map[string]json.RawMessage
	// raw holds the entry as the user wrote it when a modeled field has a value
	// Entire does not model: a fractional timeout, or an explicit 0 that would
	// otherwise read and re-encode as no timeout. It is written back unchanged,
	// since re-encoding the decoded fields would lose that value.
	raw json.RawMessage
}

// claudeHookEntryFields is ClaudeHookEntry without its JSON methods.
type claudeHookEntryFields ClaudeHookEntry

func (e *ClaudeHookEntry) UnmarshalJSON(data []byte) error {
	// encoding/json reuses slice elements without zeroing them, so bytes kept
	// from a previous decode must not survive into this one.
	*e = ClaudeHookEntry{}
	extra, present, err := unmarshalKeepingExtra(data, (*claudeHookEntryFields)(e), "type", "command", "timeout")
	e.extra = extra
	if typeErr := (*json.UnmarshalTypeError)(nil); errors.As(err, &typeErr) {
		// The other modeled fields still decoded, so Entire can recognise its own
		// hook; only the write-back has to use the original bytes.
		e.raw = slices.Clone(data)
		return nil
	}
	if err == nil && e.Timeout == 0 && slices.Contains(present, "timeout") {
		e.raw = slices.Clone(data)
	}
	return err
}

func (e ClaudeHookEntry) MarshalJSON() ([]byte, error) {
	if e.raw != nil {
		return e.raw, nil
	}
	return marshalWithExtra(claudeHookEntryFields(e), e.extra)
}

// unmarshalKeepingExtra decodes the knownKeys of data into known and returns
// every other key, plus the knownKeys data contained. Entire rewrites
// settings.json, which the user and Claude Code own, so every field it does not
// model must survive the round trip.
//
// Only exact knownKeys are decoded: encoding/json matches field names
// case-insensitively, so decoding data whole would read a key such as "Command",
// which Claude Code ignores, into the live command field.
func unmarshalKeepingExtra(data []byte, known any, knownKeys ...string) (extra map[string]json.RawMessage, present []string, err error) {
	if err := json.Unmarshal(data, &extra); err != nil {
		return nil, nil, err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
	}
	modeled := make(map[string]json.RawMessage, len(knownKeys))
	for _, k := range knownKeys {
		if v, ok := extra[k]; ok {
			modeled[k] = v
			present = append(present, k)
			delete(extra, k)
		}
	}
	modeledJSON, err := json.Marshal(modeled)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
	}
	if err := json.Unmarshal(modeledJSON, known); err != nil {
		return nil, nil, err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
	}
	return extra, present, nil
}

// marshalWithExtra encodes known, then appends extra in key order, so modeled
// fields keep their usual position and output stays deterministic.
func marshalWithExtra(known any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := jsonutil.MarshalWithNoHTMLEscape(known)
	if err != nil || len(extra) == 0 {
		return data, err //nolint:wrapcheck // returned from MarshalJSON; encoding/json adds context
	}
	var buf bytes.Buffer
	buf.Write(data[:len(data)-1])
	needComma := len(data) > len("{}")
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		key, err := jsonutil.MarshalWithNoHTMLEscape(k)
		if err != nil {
			return nil, err //nolint:wrapcheck // returned from MarshalJSON; encoding/json adds context
		}
		if needComma {
			buf.WriteByte(',')
		}
		needComma = true
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(extra[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// sessionInfoRaw is the JSON structure from SessionStart/SessionEnd/Stop hooks.
// SessionStart includes a "model" field with the LLM model identifier.
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model,omitempty"`
}

// userPromptSubmitRaw is the JSON structure from UserPromptSubmit hooks.
// Unlike other session hooks, this includes the user's prompt text.
type userPromptSubmitRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

// taskHookInputRaw is the JSON structure from PreToolUse[Task] hook
type taskHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// postToolHookInputRaw is the JSON structure from PostToolUse hooks
type postToolHookInputRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   struct {
		AgentID string `json:"agentId"`
		// Status and IsAsync report how Claude Code actually ran an Agent
		// call: "completed" once a foreground subagent finished,
		// "async_launched" (with isAsync) when it returned at launch. A Skill
		// call reports "forked" when the skill ran in an agent of its own.
		Status  string `json:"status"`
		IsAsync bool   `json:"isAsync"`
		// Background reports whether a forked skill's agent is still running.
		// A pointer, because only an explicit false means it finished.
		Background *bool `json:"background"`
	} `json:"tool_response"`
}

// skillToolName is the tool Claude Code runs skills with.
const skillToolName = "Skill"

// skillToolStatusForked is the Skill tool_response.status for a skill with
// `context: fork`, which runs in an agent of its own (Claude Code 2.1.291).
const skillToolStatusForked = "forked"

// Agent tool_response.status values that identify the launch mode.
const (
	agentToolStatusCompleted     = "completed"
	agentToolStatusAsyncLaunched = "async_launched"
)

// subagentStopHookInputRaw is the JSON structure from the SubagentStop hook.
// Per the Agent SDK docs this also carries hook_event_name and cwd, which
// entire has no use for and so doesn't parse. agent_transcript_path is
// parsed defensively: an absent field just leaves AgentTranscriptPath empty
// rather than erroring, and the lifecycle layer then falls back to resolving
// the subagent transcript from AgentID.
type subagentStopHookInputRaw struct {
	SessionID           string `json:"session_id"`
	TranscriptPath      string `json:"transcript_path"`
	AgentID             string `json:"agent_id"`
	AgentType           string `json:"agent_type"`
	AgentTranscriptPath string `json:"agent_transcript_path"`
	ToolUseID           string `json:"tool_use_id"`
}

// subagentStartHookInputRaw is the JSON structure from the SubagentStart hook.
// It names the subagent and its type but not the tool call that launched it:
// for Workflow agents there is none of their own, only the Workflow call that
// launched every agent in the run.
type subagentStartHookInputRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	AgentID        string `json:"agent_id"`
	AgentType      string `json:"agent_type"`
}

// workflowAgentType is the agent_type Claude Code reports for agents a
// Workflow launches (observed in 2.1.291).
const workflowAgentType = "workflow-subagent"

// Tool names used in Claude Code transcripts
const (
	ToolWrite        = "Write"
	ToolEdit         = "Edit"
	ToolNotebookEdit = "NotebookEdit"
	ToolMCPWrite     = "mcp__acp__Write" //nolint:gosec // G101: This is a tool name, not a credential
	ToolMCPEdit      = "mcp__acp__Edit"
)

// FileModificationTools lists tools that create or modify files
var FileModificationTools = []string{
	ToolWrite,
	ToolEdit,
	ToolNotebookEdit,
	ToolMCPWrite,
	ToolMCPEdit,
}

// messageUsage represents token usage from a Claude API response.
// This is specific to Claude/Anthropic's API format.
type messageUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// messageWithUsage represents an assistant message with usage data.
// Used for extracting token counts from Claude Code transcripts.
type messageWithUsage struct {
	ID    string       `json:"id"`
	Usage messageUsage `json:"usage"`
}
