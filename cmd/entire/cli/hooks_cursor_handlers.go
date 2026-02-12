// hooks_cursor_handlers.go contains Cursor-specific hook handler implementations.
// Cursor uses .cursor/hooks.json and sends payloads that are parsed by agent/cursor.
package cli

import (
	"context"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

func handleCursorSessionStart() error {
	return handleSessionStartCommon()
}

func handleCursorSessionEnd() error {
	hookData, err := parseHookInputWithType(agent.HookSessionEnd, os.Stdin, "session-end")
	if err != nil {
		return err
	}
	logging.Info(logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), hookData.agent.Name()), "session-end",
		slog.String("hook", "session-end"),
		slog.String("hook_type", "agent"),
		slog.String("model_session_id", hookData.input.SessionID),
	)
	return handleSessionEndFromInput(hookData.agent, hookData.input)
}

func handleCursorBeforeSubmitPrompt() error {
	hookData, err := parseHookInputWithType(agent.HookUserPromptSubmit, os.Stdin, "before-submit-prompt")
	if err != nil {
		return err
	}
	return captureInitialStateFromInput(hookData.agent, hookData.input)
}

func handleCursorStop() error {
	hookData, err := parseHookInputWithType(agent.HookStop, os.Stdin, "stop")
	if err != nil {
		return err
	}
	return commitWithMetadataFromInput(hookData.agent, hookData.input)
}

func handleCursorPreTask() error {
	hookData, err := parseHookInputWithType(agent.HookPreToolUse, os.Stdin, "pre-task")
	if err != nil {
		return err
	}
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), hookData.agent.Name())
	logging.Info(logCtx, "pre-task",
		slog.String("hook", "pre-task"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", hookData.input.SessionID),
		slog.String("transcript_path", hookData.input.SessionRef),
		slog.String("tool_use_id", hookData.input.ToolUseID),
	)
	return handlePreTaskFromInput(hookData.agent, hookData.input)
}

func handleCursorPostTask() error {
	hookData, err := parseHookInputWithType(agent.HookPostToolUse, os.Stdin, "post-task")
	if err != nil {
		return err
	}
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), hookData.agent.Name())
	logging.Info(logCtx, "post-task",
		slog.String("hook", "post-task"),
		slog.String("hook_type", "subagent"),
		slog.String("tool_use_id", hookData.input.ToolUseID),
	)
	return handlePostTaskFromInput(hookData.agent, hookData.input)
}

func handleCursorPostTodo() error {
	hookData, err := parseHookInputWithType(agent.HookPostToolUse, os.Stdin, "post-todo")
	if err != nil {
		return err
	}
	logCtx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), hookData.agent.Name())
	logging.Info(logCtx, "post-todo",
		slog.String("hook", "post-todo"),
		slog.String("hook_type", "subagent"),
		slog.String("model_session_id", hookData.input.SessionID),
		slog.String("transcript_path", hookData.input.SessionRef),
		slog.String("tool_use_id", hookData.input.ToolUseID),
	)
	handlePostTodoFromInput(hookData.agent, hookData.input)
	return nil
}
