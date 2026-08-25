package grok

import "encoding/json"

// Hook payload structs.
//
// Grok's config keys are PascalCase ("SessionStart") but the stdin payload
// spells hookEventName in snake_case ("session_start"), and every other field
// is camelCase. See cmd/entire/cli/agent/grok/AGENT.md for captured examples.

// baseHookInput holds the fields every Grok hook payload carries.
// promptId is absent on session-scoped events.
type baseHookInput struct {
	HookEventName  string `json:"hookEventName"`
	SessionID      string `json:"sessionId"`
	CWD            string `json:"cwd"`
	WorkspaceRoot  string `json:"workspaceRoot"`
	Timestamp      string `json:"timestamp"`
	PermissionMode string `json:"permissionMode"`
	PromptID       string `json:"promptId"`

	// TranscriptPath is the absolute path to the session's updates.jsonl.
	// Present on user_prompt_submit, stop, session_end, and the tool events;
	// absent on session_start, which carries Source instead.
	TranscriptPath string `json:"transcriptPath"`
}

type sessionStartInput struct {
	baseHookInput

	// Source is the start reason ("new", ...) and is the matcher field for
	// this event.
	Source string `json:"source"`
}

type userPromptSubmitInput struct {
	baseHookInput

	Prompt string `json:"prompt"`
}

// stopInput covers the Stop hook. Grok fires it twice per session: once for
// the turn (reason "end_turn", promptId set) and once at teardown (reason
// "shutdown", no promptId). Only the first is a TurnEnd — see isTurnStop.
type stopInput struct {
	baseHookInput

	Reason               string `json:"reason"`
	StopHookActive       bool   `json:"stopHookActive"`
	LastAssistantMessage string `json:"lastAssistantMessage"`
	SubagentType         string `json:"subagentType"`
}

// stopCancelledInput covers a turn interrupted, refused, or capped. Grok fires
// this INSTEAD of Stop, so it must still produce a TurnEnd or the turn's work
// is never checkpointed.
type stopCancelledInput struct {
	baseHookInput

	Reason        string `json:"reason"`
	CancelledBy   string `json:"cancelledBy"`
	CancelTrigger string `json:"cancelTrigger"`
	ReasonDetails string `json:"reasonDetails"`
	SubagentType  string `json:"subagentType"`
}

// stopFailureInput covers a turn ended by an API error. Also fires instead of
// Stop.
type stopFailureInput struct {
	baseHookInput

	Error        string `json:"error"`
	ErrorDetails string `json:"errorDetails"`
	SubagentType string `json:"subagentType"`
}

type sessionEndInput struct {
	baseHookInput

	Reason string `json:"reason"`
	// SubagentType is set when the ending session is a child session.
	SubagentType string `json:"subagentType"`
}

type compactInput struct {
	baseHookInput

	// Trigger is "manual" or "auto".
	Trigger string `json:"trigger"`
}

type subagentInput struct {
	baseHookInput

	SubagentType string `json:"subagentType"`
	Phase        string `json:"phase"`
}

type toolUseInput struct {
	baseHookInput

	ToolName   string          `json:"toolName"`
	ToolUseID  string          `json:"toolUseId"`
	ToolInput  json.RawMessage `json:"toolInput"`
	ToolResult json.RawMessage `json:"toolResult"`
}

// toolInputPaths are the file-path fields Grok's write/edit tools carry in
// toolInput.
type toolInputPaths struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
}

// toolResultPaths picks the absolute path out of a write/edit tool result,
// which is preferable to toolInput's repo-relative one.
type toolResultPaths struct {
	EditsApplied struct {
		AbsolutePath string `json:"absolute_path"`
	} `json:"EditsApplied"`
}

// Transcript envelope.
//
// updates.jsonl is NOT a stream of bare events: each line is a JSON-RPC
// envelope whose payload sits at params.update. Reading sessionUpdate off the
// top level yields nothing.

type transcriptLine struct {
	Timestamp int64  `json:"timestamp"`
	Method    string `json:"method"`
	Params    struct {
		SessionID string           `json:"sessionId"`
		Update    transcriptUpdate `json:"update"`
	} `json:"params"`
}

type transcriptUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`

	// tool_call / tool_call_update
	ToolCallID string      `json:"toolCallId"`
	Status     string      `json:"status"`
	Kind       string      `json:"kind"`
	Title      string      `json:"title"`
	Locations  []updateLoc `json:"locations"`

	// Content is deliberately raw: Grok overloads the key with two different
	// shapes. On tool_call_update it is an ARRAY of blocks; on
	// user_message_chunk / agent_message_chunk it is a single OBJECT
	// ({"type":"text","text":...}). Typing it as a slice makes every message
	// chunk fail to unmarshal, and since the readers skip unparseable lines the
	// failure is silent — it cost the model lookup, which lives in a chunk's
	// _meta. Decode it with diffBlocks() instead.
	Content json.RawMessage `json:"content"`

	// user_message_chunk / agent_message_chunk
	Meta struct {
		ModelID string `json:"modelId"`
	} `json:"_meta"`

	// turn_completed
	PromptID   string          `json:"prompt_id"`
	StopReason string          `json:"stop_reason"`
	Usage      *transcriptUsge `json:"usage"`
}

// updateContent is one content block on a tool_call_update. A block of type
// "diff" is the authoritative signal that a file changed, and carries the
// absolute path.
type updateContent struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type updateLoc struct {
	Path string `json:"path"`
}

// diffBlocks returns the content blocks when Content holds an array, and nil
// for the single-object (message chunk) shape.
func (u transcriptUpdate) diffBlocks() []updateContent {
	if len(u.Content) == 0 {
		return nil
	}
	var blocks []updateContent
	if err := json.Unmarshal(u.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// transcriptUsge is turn_completed's usage block.
//
// InputTokens is the TOTAL input, cache inclusive — the fresh portion is
// InputTokens - CachedReadTokens - CacheCreationTokens. Verified against the
// same turn's headless JSON summary, which reports the fresh figure directly.
type transcriptUsge struct {
	InputTokens         int `json:"inputTokens"`
	OutputTokens        int `json:"outputTokens"`
	TotalTokens         int `json:"totalTokens"`
	CachedReadTokens    int `json:"cachedReadTokens"`
	CacheCreationTokens int `json:"cacheCreationTokens"`
	ReasoningTokens     int `json:"reasoningTokens"`
	ModelCalls          int `json:"modelCalls"`
}
