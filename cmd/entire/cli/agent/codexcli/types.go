package codexcli

import "encoding/json"

// Top-level JSONL event types emitted by codex exec --json.
const (
	EventThreadStarted = "thread.started"
	EventTurnStarted   = "turn.started"
	EventTurnCompleted = "turn.completed"
	EventTurnFailed    = "turn.failed"
	EventItemStarted   = "item.started"
	EventItemUpdated   = "item.updated"
	EventItemCompleted = "item.completed"
	EventError         = "error"
)

// Item type constants within item events.
const (
	ItemAgentMessage     = "agent_message"
	ItemReasoning        = "reasoning"
	ItemCommandExecution = "command_execution"
	ItemFileChange       = "file_change"
	ItemMCPToolCall      = "mcp_tool_call"
	ItemWebSearch        = "web_search"
	ItemTodoList         = "todo_list"
	ItemError            = "error"
)

// File change kind constants.
const (
	FileChangeAdd    = "add"
	FileChangeUpdate = "update"
	FileChangeDelete = "delete"
)

// Event is the top-level envelope for all Codex JSONL events.
type Event struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id,omitempty"`
	Usage    *TurnUsage      `json:"usage,omitempty"`
	Error    *ErrorDetail    `json:"error,omitempty"`
	Message  string          `json:"message,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
}

// TurnUsage contains token counts emitted with turn.completed events.
type TurnUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// ErrorDetail is the error payload in turn.failed or error events.
type ErrorDetail struct {
	Message string `json:"message"`
}

// ItemEnvelope extracts the common fields from an item payload.
type ItemEnvelope struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status,omitempty"`
}

// AgentMessageItem is an item with type "agent_message".
type AgentMessageItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}

// ReasoningItem is an item with type "reasoning".
type ReasoningItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}

// CommandExecutionItem is an item with type "command_execution".
type CommandExecutionItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Status           string `json:"status"`
}

// FileChangeItem is an item with type "file_change".
type FileChangeItem struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Changes []FileChange `json:"changes"`
	Status  string       `json:"status"`
}

// FileChange represents a single file modification within a file_change item.
type FileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// MCPToolCallItem is an item with type "mcp_tool_call".
type MCPToolCallItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *string         `json:"error,omitempty"`
	Status    string          `json:"status"`
}

// TodoListItem is an item with type "todo_list".
type TodoListItem struct {
	ID    string     `json:"id"`
	Type  string     `json:"type"`
	Items []TodoItem `json:"items"`
}

// TodoItem is a single entry in a todo_list item.
type TodoItem struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

// HookNameSessionStart is the hook name for codex session start (used internally).
const HookNameSessionStart = "session-start"

// HookNameSessionEnd is the hook name for codex session end (used internally).
const HookNameSessionEnd = "session-end"
