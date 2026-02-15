// Package codexcli implements the Agent interface for OpenAI Codex CLI.
package codexcli

import "encoding/json"

// Codex CLI hook names - these become subcommands under `entire hooks codex`
const (
	HookNameTurnComplete = "turn-complete"
)

// notifyPayload is the JSON structure received from Codex's notify hook
// when the agent-turn-complete event fires.
type notifyPayload struct {
	Type                 string   `json:"type"`
	TurnID               string   `json:"turn-id"`
	ThreadID             string   `json:"thread-id"`
	InputMessages        []string `json:"input-messages"`
	LastAssistantMessage string   `json:"last-assistant-message"`
}

// Codex transcript JSONL event types
const (
	eventTypeSessionMeta  = "session_meta"
	eventTypeResponseItem = "response_item"
	eventTypeEventMsg     = "event_msg"
	eventTypeTurnContext  = "turn_context"
)

// Codex event_msg subtypes
const (
	eventMsgUserMessage    = "user_message"
	eventMsgAgentMessage   = "agent_message"
	eventMsgAgentReasoning = "agent_reasoning"
	eventMsgTaskStarted    = "task_started"
	eventMsgTaskComplete   = "task_complete"
	eventMsgTokenCount     = "token_count"
	eventMsgTurnAborted    = "turn_aborted"
)

// Codex response_item subtypes
const (
	responseItemMessage         = "message"
	responseItemFunctionCall    = "function_call"
	responseItemFunctionCallOut = "function_call_output"
	responseItemReasoning       = "reasoning"
)

// TranscriptLine represents a single line in a Codex JSONL transcript.
type TranscriptLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// sessionMetaPayload is the payload for session_meta events.
type sessionMetaPayload struct {
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	CWD        string `json:"cwd"`
	Originator string `json:"originator"`
	CLIVersion string `json:"cli_version"`
	Model      string `json:"model_provider"`
	Git        struct {
		CommitHash    string `json:"commit_hash"`
		Branch        string `json:"branch"`
		RepositoryURL string `json:"repository_url"`
	} `json:"git"`
}

// eventMsgPayload is the payload for event_msg events.
type eventMsgPayload struct {
	Type    string          `json:"type"`
	Message string          `json:"message,omitempty"`
	TurnID  string          `json:"turn_id,omitempty"`
	Info    json.RawMessage `json:"info,omitempty"`
}

// tokenCountInfo holds token usage info from a token_count event.
type tokenCountInfo struct {
	TotalTokenUsage struct {
		InputTokens           int `json:"input_tokens"`
		CachedInputTokens     int `json:"cached_input_tokens"`
		OutputTokens          int `json:"output_tokens"`
		ReasoningOutputTokens int `json:"reasoning_output_tokens"`
		TotalTokens           int `json:"total_tokens"`
	} `json:"total_token_usage"`
}

// responseItemPayload is the payload for response_item events.
type responseItemPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Output    string          `json:"output,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Phase     string          `json:"phase,omitempty"`
}

// execCommandArgs represents the parsed arguments of an exec_command function call.
type execCommandArgs struct {
	Cmd     string `json:"cmd"`
	Workdir string `json:"workdir,omitempty"`
}

// fileModifyingPatterns are shell command patterns that indicate file modifications.
// Codex uses exec_command with shell commands instead of dedicated Write/Edit tools.
var fileModifyingPatterns = []string{
	"apply_patch",
	"cat >",
	"cat >>",
	"tee ",
	"sed -i",
	"> ",
	">> ",
	"mv ",
	"cp ",
	"mkdir -p",
	"touch ",
	"rm ",
	"echo ",
}
