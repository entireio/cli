// hook_registry.go provides hook command registration for agents.
// The lifecycle dispatcher (DispatchLifecycleEvent) handles all lifecycle events.
// PostTodo is the only hook that's handled directly (not via lifecycle dispatcher).
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

// agentHookLogCleanup stores the cleanup function for agent hook logging.
// Set by PersistentPreRunE, called by PersistentPostRunE.
var agentHookLogCleanup func()

// currentHookAgentName stores the agent name for the currently executing hook.
// Set by newAgentHookVerbCmdWithLogging before calling the handler.
// This allows handlers to know which agent invoked the hook without guessing.
var currentHookAgentName agent.AgentName

// GetCurrentHookAgent returns the agent for the currently executing hook.
// Returns the agent based on the hook command structure (e.g., "entire hooks claude-code ...")
// rather than guessing from directory presence.
// Falls back to GetAgent() if not in a hook context.
func GetCurrentHookAgent() (agent.Agent, error) {
	if currentHookAgentName == "" {
		return nil, errors.New("not in a hook context: agent name not set")
	}

	ag, err := agent.Get(currentHookAgentName)
	if err != nil {
		return nil, fmt.Errorf("getting hook agent %q: %w", currentHookAgentName, err)
	}
	return ag, nil
}

// newAgentHooksCmd creates a hooks subcommand for an agent.
// It dynamically creates subcommands for each hook the agent supports.
func newAgentHooksCmd(agentName agent.AgentName, handler agent.Agent) *cobra.Command {
	cmd := &cobra.Command{
		Use:    string(agentName),
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

	for _, hookName := range handler.HookNames() {
		cmd.AddCommand(newAgentHookVerbCmdWithLogging(agentName, hookName))
	}

	return cmd
}

// getHookType returns the hook type based on the hook name.
// Returns "subagent" for task-related hooks (pre-task, post-task, post-todo),
// "tool" for tool-related hooks (before-tool, after-tool),
// "agent" for all other agent hooks.
func getHookType(hookName string) string {
	switch hookName {
	case claudecode.HookNamePreTask, claudecode.HookNamePostTask, claudecode.HookNamePostTodo:
		return "subagent"
	case geminicli.HookNameBeforeTool, geminicli.HookNameAfterTool:
		return "tool"
	default:
		return "agent"
	}
}

// newAgentHookVerbCmdWithLogging creates a command for a specific hook verb with structured logging.
// It uses the lifecycle dispatcher (ParseHookEvent → DispatchLifecycleEvent) as the primary path.
// PostTodo is handled directly as it's Claude-specific and not part of the lifecycle dispatcher.
func newAgentHookVerbCmdWithLogging(agentName agent.AgentName, hookName string) *cobra.Command {
	return &cobra.Command{
		Use:    hookName,
		Hidden: true,
		Short:  "Called on " + hookName,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Skip silently if not in a git repository - hooks shouldn't prevent the agent from working
			if _, err := paths.RepoRoot(); err != nil {
				return nil
			}

			// Skip if Entire is not enabled
			enabled, err := IsEnabled()
			if err == nil && !enabled {
				return nil
			}

			start := time.Now()

			// Initialize logging context with agent name
			ctx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), agentName)

			// Get strategy name for logging
			strategyName := GetStrategy().Name()

			hookType := getHookType(hookName)

			logging.Debug(ctx, "hook invoked",
				slog.String("hook", hookName),
				slog.String("hook_type", hookType),
				slog.String("strategy", strategyName),
			)

			// Set the current hook agent so handlers can retrieve it
			currentHookAgentName = agentName
			defer func() { currentHookAgentName = "" }()

			// Use the lifecycle dispatcher for all hooks
			var hookErr error
			ag, agentErr := agent.Get(agentName)
			if agentErr != nil {
				return fmt.Errorf("failed to get agent %q: %w", agentName, agentErr)
			}

			// Use cmd.InOrStdin() to support testing with cmd.SetIn()
			event, parseErr := ag.ParseHookEvent(hookName, cmd.InOrStdin())
			if parseErr != nil {
				return fmt.Errorf("failed to parse hook event: %w", parseErr)
			}

			if event != nil {
				// Lifecycle event — use the generic dispatcher
				hookErr = DispatchLifecycleEvent(ag, event)
			} else if agentName == agent.AgentNameClaudeCode && hookName == claudecode.HookNamePostTodo {
				// PostTodo is Claude-specific: creates incremental checkpoints during subagent execution
				hookErr = handleClaudeCodePostTodo()
			}
			// Other pass-through hooks (nil event, no special handling) are no-ops

			logging.LogDuration(ctx, slog.LevelDebug, "hook completed", start,
				slog.String("hook", hookName),
				slog.String("hook_type", hookType),
				slog.String("strategy", strategyName),
				slog.Bool("success", hookErr == nil),
			)

			return hookErr
		},
	}
}
