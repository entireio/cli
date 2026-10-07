package claudecode

import "encoding/json"

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
}

// ClaudeHookEntry represents a single hook command
type ClaudeHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	// Timeout is the hook's timeout in seconds. Omitted (0) leaves Claude Code's
	// default in place; set only where a hook needs an explicit budget.
	Timeout int `json:"timeout,omitempty"`
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
