package antigravity

import "encoding/json"

// HooksFile maps hook names to their event configurations.
// The top-level key is the hook name (user-defined, e.g. "my-linter-hook").
type HooksFile = map[string]HookConfig

// HookConfig defines the event handlers for a named hook entry.
type HookConfig struct {
	Enabled        *bool           `json:"enabled,omitempty"`
	PreToolUse     []ToolHandler   `json:"PreToolUse,omitempty"`
	PostToolUse    []ToolHandler   `json:"PostToolUse,omitempty"`
	PreInvocation  []SimpleHandler `json:"PreInvocation,omitempty"`
	PostInvocation []SimpleHandler `json:"PostInvocation,omitempty"`
	Stop           []SimpleHandler `json:"Stop,omitempty"`
}

// ToolHandler is a matcher + handlers entry used for PreToolUse / PostToolUse.
type ToolHandler struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks,omitempty"`
}

// SimpleHandler is a direct handler entry used for PreInvocation, PostInvocation, and Stop.
type SimpleHandler struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// HookCommand is a single executable hook command.
type HookCommand struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// CommonPayload contains system metadata fields present in all hook payloads.
type CommonPayload struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
}

// ToolCall represents a proposed or completed tool invocation.
type ToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// PreToolUsePayload is the stdin payload for the PreToolUse hook.
type PreToolUsePayload struct {
	CommonPayload

	ToolCall ToolCall `json:"toolCall"`
	StepIdx  int      `json:"stepIdx"`
}

// PreToolUseOutput is the stdout response for the PreToolUse hook.
type PreToolUseOutput struct {
	Decision            string   `json:"decision"` // Required: "allow", "deny", "ask", "force_ask"
	Reason              string   `json:"reason,omitempty"`
	PermissionOverrides []string `json:"permissionOverrides,omitempty"`
}

// PostToolUsePayload is the stdin payload for the PostToolUse hook.
type PostToolUsePayload struct {
	CommonPayload

	StepIdx int    `json:"stepIdx"`
	Error   string `json:"error,omitempty"`
}

// InvocationPayload is the stdin payload for PreInvocation and PostInvocation hooks.
type InvocationPayload struct {
	CommonPayload

	InvocationNum   int `json:"invocationNum"`
	InitialNumSteps int `json:"initialNumSteps"`
}

// PostInvocationPayload is an alias for InvocationPayload; both events share the same input shape.
type PostInvocationPayload = InvocationPayload

// InvocationOutput is the stdout response for PreInvocation and PostInvocation hooks.
type InvocationOutput struct {
	InjectSteps         []InjectStep `json:"injectSteps,omitempty"`
	TerminationBehavior string       `json:"terminationBehavior,omitempty"` // PostInvocation only
}

// InjectStep is a step injected into the conversation trajectory.
// Exactly one of ToolCall, UserMessage, or EphemeralMessage should be set.
type InjectStep struct {
	ToolCall         *ToolCall `json:"toolCall,omitempty"`
	UserMessage      string    `json:"userMessage,omitempty"`
	EphemeralMessage string    `json:"ephemeralMessage,omitempty"`
}

// StopPayload is the stdin payload for the Stop hook.
type StopPayload struct {
	CommonPayload

	ExecutionNum      int    `json:"executionNum"`
	TerminationReason string `json:"terminationReason"`
	Error             string `json:"error,omitempty"`
	FullyIdle         bool   `json:"fullyIdle"` // Required
}

// StopOutput is the stdout response for the Stop hook.
type StopOutput struct {
	Decision string `json:"decision"` // Required: "continue" to re-enter the loop; any other value allows stop
	Reason   string `json:"reason,omitempty"`
}
