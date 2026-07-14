package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ContextInjection carries text that Entire asks an agent to place into the
// model's context window for the current session. An empty Text means there is
// nothing to inject.
type ContextInjection struct {
	Text string
}

// ContextInjector is implemented by agents that can place additional context
// into the *model* at a specific lifecycle event — for example Pi's
// before_agent_start (TurnStart) message injection or OpenCode's
// experimental.chat.system.transform.
//
// This is deliberately distinct from HookResponseWriter: a hook response shows
// a banner to the *user*, whereas a ContextInjection reaches the *model*. An
// agent may implement both.
//
// The agent declares which lifecycle event it injects at (InjectionEvent) and
// renders the native payload its transport understands (RenderContextInjection).
// For extension-backed agents (Pi, OpenCode) that payload is written to the
// hook's stdout and the embedded extension applies it via the agent's native
// injection API.
type ContextInjector interface {
	Agent

	// InjectionEvent is the lifecycle event at which this agent emits an
	// injection payload. The dispatcher only calls RenderContextInjection on
	// matching events.
	InjectionEvent() EventType

	// RenderContextInjection returns the bytes to write to the hook's stdout to
	// inject inj into the model, in the agent's native format. Returning an
	// empty slice (or nil) means "write nothing".
	RenderContextInjection(inj ContextInjection) ([]byte, error)
}

// AsContextInjector returns ag as a ContextInjector when it implements the
// interface. Mirrors AsHookResponseWriter so callers don't type-assert inline.
func AsContextInjector(ag Agent) (ContextInjector, bool) {
	if ag == nil {
		return nil, false
	}
	ci, ok := ag.(ContextInjector)
	return ci, ok
}

// RenderAdditionalContextHookOutput renders the Claude-Code-style hook output
// that injects text into the model's context window:
//
//	{"hookSpecificOutput":{"hookEventName":<event>,"additionalContext":<text>}}
//
// Claude Code, Codex (which hosts Claude-compatible hooks) and Gemini CLI all
// consume this shape on their prompt-submit hook (UserPromptSubmit / BeforeAgent)
// and merge additionalContext into the model context. Returns (nil, nil) for
// empty text so callers can write nothing.
func RenderAdditionalContextHookOutput(hookEventName, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	type hookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	payload := struct {
		HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
	}{hookSpecificOutput{HookEventName: hookEventName, AdditionalContext: text}}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal additionalContext hook output: %w", err)
	}
	return append(b, '\n'), nil
}
