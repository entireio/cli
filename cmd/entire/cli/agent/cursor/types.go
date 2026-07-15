package cursor

import "encoding/json"

// CursorHooksFile represents the .cursor/HooksFileName structure.
// Cursor uses a flat JSON file with version and hooks sections.
//
//nolint:revive // CursorHooksFile is clearer than HooksFile when used outside this package
type CursorHooksFile struct {
	Version int         `json:"version"`
	Hooks   CursorHooks `json:"hooks"`
}

// CursorHooks contains all hook configurations using camelCase keys.
//
//nolint:revive // CursorHooks is clearer than Hooks when used outside this package
type CursorHooks struct {
	SessionStart       []CursorHookEntry `json:"sessionStart,omitempty"`
	SessionEnd         []CursorHookEntry `json:"sessionEnd,omitempty"`
	BeforeSubmitPrompt []CursorHookEntry `json:"beforeSubmitPrompt,omitempty"`
	Stop               []CursorHookEntry `json:"stop,omitempty"`
	PreCompact         []CursorHookEntry `json:"preCompact,omitempty"`
	SubagentStart      []CursorHookEntry `json:"subagentStart,omitempty"`
	SubagentStop       []CursorHookEntry `json:"subagentStop,omitempty"`
}

// CursorHookEntry represents a single hook command.
// Cursor hooks have a command string and an optional matcher field for filtering by tool name.
//
//nolint:revive // CursorHookEntry is clearer than HookEntry when used outside this package
type CursorHookEntry struct {
	Command string `json:"command"`
	Matcher string `json:"matcher,omitempty"`
}

// sessionStartRaw is the JSON structure from SessionStart hooks.
// IDE includes composer_mode ("agent"), CLI omits it.
// IDE model is "default", CLI has actual model name.
type sessionStartRaw struct {
	// common
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	Model          string   `json:"model"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	UserEmail      string   `json:"user_email"`
	TranscriptPath string   `json:"transcript_path"`

	// hook specific
	IsBackgroundAgent bool   `json:"is_background_agent"`
	ComposerMode      string `json:"composer_mode"` // IDE-only: "agent"
}

// stopHookInputRaw is the JSON structure from Stop hooks.
// IDE provides transcript_path; CLI sends null.
// Both provide status and loop_count.
//
// Token fields (input_tokens, output_tokens, cache_read_tokens,
// cache_write_tokens) are reported per turn by recent Cursor versions and are
// the only authoritative source of token accounting — the JSONL transcript
// does not include usage data.
type stopHookInputRaw struct {
	// common
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	Model          string   `json:"model"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	UserEmail      string   `json:"user_email"`
	TranscriptPath string   `json:"transcript_path"`

	// hook specific
	Status           string      `json:"status"`
	LoopCount        json.Number `json:"loop_count"`
	InputTokens      json.Number `json:"input_tokens"`       // Total input tokens (includes cache portions)
	OutputTokens     json.Number `json:"output_tokens"`      // Generated output tokens
	CacheReadTokens  json.Number `json:"cache_read_tokens"`  // Tokens served from cache (subset of input_tokens)
	CacheWriteTokens json.Number `json:"cache_write_tokens"` // Tokens written to cache (subset of input_tokens)
}

// sessionEndRaw is the JSON structure from SessionEnd hooks.
// IDE provides transcript_path; CLI sends null.
// Both provide reason, duration_ms, is_background_agent, final_status.
type sessionEndRaw struct {
	// common
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	Model          string   `json:"model"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	UserEmail      string   `json:"user_email"`
	TranscriptPath string   `json:"transcript_path"`

	// hook specific
	Reason            string      `json:"reason"`
	DurationMs        json.Number `json:"duration_ms"`
	IsBackgroundAgent bool        `json:"is_background_agent"`
	FinalStatus       string      `json:"final_status"`
}

// beforeSubmitPromptInputRaw is the JSON structure from BeforeSubmitPrompt hooks.
type beforeSubmitPromptInputRaw struct {
	// common
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	Model          string   `json:"model"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	UserEmail      string   `json:"user_email"`
	TranscriptPath string   `json:"transcript_path"`

	// hook specific
	Prompt string `json:"prompt"`
}

// preCompactHookInputRaw is the JSON structure from PreCompact hook.
type preCompactHookInputRaw struct {
	// common
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	Model          string   `json:"model"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	UserEmail      string   `json:"user_email"`
	TranscriptPath string   `json:"transcript_path"`

	// hook specific
	Trigger             string      `json:"trigger"`               // "auto" | "manual",
	ContextUsagePercent json.Number `json:"context_usage_percent"` // : 85,
	ContextTokens       json.Number `json:"context_tokens"`        // 120000,
	ContextWindowSize   json.Number `json:"context_window_size"`   // : 128000,
	MessageCount        json.Number `json:"message_count"`         // 45,
	MessagesToCompact   json.Number `json:"messages_to_compact"`   // : 30,
	IsFirstCompaction   bool        `json:"is_first_compaction"`   // true | false
}

// subagentStartHookInputRaw is the JSON structure from SubagentStart[Task] hook.
type subagentStartHookInputRaw struct {
	// common
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	Model          string   `json:"model"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	UserEmail      string   `json:"user_email"`
	TranscriptPath string   `json:"transcript_path"`

	// hook specific
	SubagentID           string `json:"subagent_id"`
	SubagentType         string `json:"subagent_type"`
	SubagentModel        string `json:"subagent_model"`
	Task                 string `json:"task"`
	ParentConversationID string `json:"parent_conversation_id"`
	ToolCallID           string `json:"tool_call_id"`
	IsParallelWorker     bool   `json:"is_parallel_worker"`
}

// subagentStopHookInputRaw is the JSON structure from SubagentStop hooks.
type subagentStopHookInputRaw struct {
	// common
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	Model          string   `json:"model"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	UserEmail      string   `json:"user_email"`
	TranscriptPath string   `json:"transcript_path"`

	// hook specific
	SubagentID           string      `json:"subagent_id"`
	SubagentType         string      `json:"subagent_type"`
	Status               string      `json:"status"`
	Duration             json.Number `json:"duration_ms"`
	Summary              string      `json:"summary"`
	ParentConversationID string      `json:"parent_conversation_id"`
	MessageCount         json.Number `json:"message_count"`
	ToolCallCount        json.Number `json:"tool_call_count"`
	ModifiedFiles        []string    `json:"modified_files"`
	LoopCount            json.Number `json:"loop_count"`
	Task                 string      `json:"task"`
	Description          string      `json:"description"`
	AgentTranscriptPath  string      `json:"agent_transcript_path"`
}
