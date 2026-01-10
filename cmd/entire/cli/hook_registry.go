package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/agent/claudecode"
	"entire.io/cli/cmd/entire/cli/agent/opencode"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

// HookHandlerFunc is a function that handles a specific hook event.
type HookHandlerFunc func() error

// hookRegistry maps (agentName, hookName) to handler functions.
// This allows agents to define their hook vocabulary while keeping
// handler logic in the CLI package (avoiding circular dependencies).
var hookRegistry = map[string]map[string]HookHandlerFunc{}

// RegisterHookHandler registers a handler for an agent's hook.
func RegisterHookHandler(agentName, hookName string, handler HookHandlerFunc) {
	if hookRegistry[agentName] == nil {
		hookRegistry[agentName] = make(map[string]HookHandlerFunc)
	}
	hookRegistry[agentName][hookName] = handler
}

// registerHookWithEnabledCheck registers a handler that skips execution when Entire is disabled.
// This is a convenience wrapper that avoids repeating the IsEnabled check in every handler.
func registerHookWithEnabledCheck(agentName, hookName string, handler HookHandlerFunc) {
	RegisterHookHandler(agentName, hookName, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handler()
	})
}

// GetHookHandler returns the handler for an agent's hook, or nil if not found.
func GetHookHandler(agentName, hookName string) HookHandlerFunc {
	if handlers, ok := hookRegistry[agentName]; ok {
		return handlers[hookName]
	}
	return nil
}

// init registers hook handlers for all supported agents.
// Each handler automatically checks if Entire is enabled before executing.
//
//nolint:gochecknoinits // Hook handler registration at startup is the intended pattern
func init() {
	// Register Claude Code handlers
	registerHookWithEnabledCheck(agent.AgentNameClaudeCode, claudecode.HookNameSessionStart, handleSessionStart)
	registerHookWithEnabledCheck(agent.AgentNameClaudeCode, claudecode.HookNameStop, commitWithMetadata)
	registerHookWithEnabledCheck(agent.AgentNameClaudeCode, claudecode.HookNameUserPromptSubmit, captureInitialState)
	registerHookWithEnabledCheck(agent.AgentNameClaudeCode, claudecode.HookNamePreTask, handlePreTask)
	registerHookWithEnabledCheck(agent.AgentNameClaudeCode, claudecode.HookNamePostTask, handlePostTask)
	registerHookWithEnabledCheck(agent.AgentNameClaudeCode, claudecode.HookNamePostTodo, handlePostTodo)

	// Register OpenCode handlers
	registerHookWithEnabledCheck(agent.AgentNameOpenCode, opencode.HookNameSessionStart, handleOpencodeSessionStart)
	registerHookWithEnabledCheck(agent.AgentNameOpenCode, opencode.HookNameStop, handleOpencodeStop)
}

// agentHookLogCleanup stores the cleanup function for agent hook logging.
// Set by PersistentPreRunE, called by PersistentPostRunE.
var agentHookLogCleanup func()

// newAgentHooksCmd creates a hooks subcommand for an agent that implements HookHandler.
// It dynamically creates subcommands for each hook the agent supports.
func newAgentHooksCmd(agentName string, handler agent.HookHandler) *cobra.Command {
	cmd := &cobra.Command{
		Use:    agentName,
		Short:  handler.Description() + " hook handlers",
		Hidden: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			agentHookLogCleanup = initHookLogging()
			return nil
		},
		PersistentPostRunE: func(_ *cobra.Command, _ []string) error {
			if agentHookLogCleanup != nil {
				agentHookLogCleanup()
			}
			return nil
		},
	}

	for _, hookName := range handler.GetHookNames() {
		cmd.AddCommand(newAgentHookVerbCmdWithLogging(agentName, hookName))
	}

	return cmd
}

// getHookType returns the hook type based on the hook name.
// Returns "subagent" for task-related hooks (pre-task, post-task, post-todo),
// "agent" for all other agent hooks.
func getHookType(hookName string) string {
	switch hookName {
	case claudecode.HookNamePreTask, claudecode.HookNamePostTask, claudecode.HookNamePostTodo:
		return "subagent"
	default:
		return "agent"
	}
}

// newAgentHookVerbCmdWithLogging creates a command for a specific hook verb with structured logging.
// It logs hook invocation at DEBUG level and completion with duration at INFO level.
func newAgentHookVerbCmdWithLogging(agentName, hookName string) *cobra.Command {
	return &cobra.Command{
		Use:   hookName,
		Short: "Called on " + hookName,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Skip silently if not in a git repository - hooks shouldn't prevent the agent from working
			if _, err := paths.RepoRoot(); err != nil {
				return nil //nolint:nilerr // intentional silent skip when no git repo
			}

			start := time.Now()

			// Initialize logging context
			ctx := logging.WithComponent(context.Background(), "hooks")

			// Get strategy name for logging
			strategyName := GetStrategy().Name()

			hookType := getHookType(hookName)

			logging.Debug(ctx, "hook invoked",
				slog.String("hook", hookName),
				slog.String("hook_type", hookType),
				slog.String("agent", agentName),
				slog.String("strategy", strategyName),
			)

			handler := GetHookHandler(agentName, hookName)
			if handler == nil {
				logging.Error(ctx, "no handler registered",
					slog.String("hook", hookName),
					slog.String("hook_type", hookType),
					slog.String("agent", agentName),
				)
				return fmt.Errorf("no handler registered for %s/%s", agentName, hookName)
			}

			hookErr := handler()

			logging.LogDuration(ctx, slog.LevelDebug, "hook completed", start,
				slog.String("hook", hookName),
				slog.String("hook_type", hookType),
				slog.String("agent", agentName),
				slog.String("strategy", strategyName),
				slog.Bool("success", hookErr == nil),
			)

			return hookErr
		},
	}
}
