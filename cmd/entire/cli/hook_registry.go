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
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

// HookHandlerFunc is a function that handles a specific hook event.
type HookHandlerFunc func() error

// hookRegistry maps (agentName, hookName) to handler functions.
// This allows agents to define their hook vocabulary while keeping
// handler logic in the CLI package (avoiding circular dependencies).
var hookRegistry = map[agent.AgentName]map[string]HookHandlerFunc{}

// RegisterHookHandler registers a handler for an agent's hook.
func RegisterHookHandler(agentName agent.AgentName, hookName string, handler HookHandlerFunc) {
	if hookRegistry[agentName] == nil {
		hookRegistry[agentName] = make(map[string]HookHandlerFunc)
	}
	hookRegistry[agentName][hookName] = handler
}

// GetHookHandler returns the handler for an agent's hook, or nil if not found.
func GetHookHandler(agentName agent.AgentName, hookName string) HookHandlerFunc {
	if handlers, ok := hookRegistry[agentName]; ok {
		return handlers[hookName]
	}
	return nil
}

// init registers Claude Code hook handlers.
// Each handler checks if Entire is enabled before executing.
//
//nolint:gochecknoinits // Hook handler registration at startup is the intended pattern
func init() {
	// Register Claude Code handlers
	RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNameSessionStart, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleClaudeCodeSessionStart()
	})

	RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNameSessionEnd, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleClaudeCodeSessionEnd()
	})

	RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNameStop, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return commitWithMetadata()
	})

	RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNameUserPromptSubmit, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return captureInitialState()
	})

	RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNamePreTask, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleClaudeCodePreTask()
	})

	RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNamePostTask, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleClaudeCodePostTask()
	})

	RegisterHookHandler(agent.AgentNameClaudeCode, claudecode.HookNamePostTodo, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleClaudeCodePostTodo()
	})

	// Register Gemini CLI handlers
	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameSessionStart, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiSessionStart()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameSessionEnd, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiSessionEnd()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameBeforeTool, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiBeforeTool()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameAfterTool, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiAfterTool()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameBeforeAgent, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiBeforeAgent()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameAfterAgent, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiAfterAgent()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameBeforeModel, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiBeforeModel()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameAfterModel, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiAfterModel()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameBeforeToolSelection, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiBeforeToolSelection()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNamePreCompress, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiPreCompress()
	})

	RegisterHookHandler(agent.AgentNameGemini, geminicli.HookNameNotification, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handleGeminiNotification()
	})

	registerPiHookHandlers()
}

func registerPiHookHandlers() {
	RegisterHookHandler(agent.AgentNamePi, pi.HookNameSessionStart, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handlePiSessionStart()
	})

	RegisterHookHandler(agent.AgentNamePi, pi.HookNameSessionEnd, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handlePiSessionEnd()
	})

	RegisterHookHandler(agent.AgentNamePi, pi.HookNameUserPromptSubmit, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handlePiUserPromptSubmit()
	})

	RegisterHookHandler(agent.AgentNamePi, pi.HookNameBeforeTool, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handlePiBeforeTool()
	})

	RegisterHookHandler(agent.AgentNamePi, pi.HookNameAfterTool, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handlePiAfterTool()
	})

	RegisterHookHandler(agent.AgentNamePi, pi.HookNameStop, func() error {
		enabled, err := IsEnabled()
		if err == nil && !enabled {
			return nil
		}
		return handlePiStop()
	})
}

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
//

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

// newAgentHooksCmd creates a hooks subcommand for an agent that implements HookHandler.
// It dynamically creates subcommands for each hook the agent supports.
func newAgentHooksCmd(agentName agent.AgentName, handler agent.HookHandler) *cobra.Command {
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

	for _, hookName := range handler.GetHookNames() {
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
// It logs hook invocation at DEBUG level and completion with duration at INFO level.
func newAgentHookVerbCmdWithLogging(agentName agent.AgentName, hookName string) *cobra.Command {
	return &cobra.Command{
		Use:    hookName,
		Hidden: true,
		Short:  "Called on " + hookName,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Skip silently if not in a git repository - hooks shouldn't prevent the agent from working
			if _, err := paths.RepoRoot(); err != nil {
				return nil
			}

			start := time.Now()

			// Initialize logging context with agent name
			ctx := logging.WithAgent(logging.WithComponent(context.Background(), "hooks"), agentName)

			// Get strategy name for logging
			strategyName := unknownStrategyName //nolint:ineffassign,wastedassign // already present in codebase
			strategyName = GetStrategy().Name()

			hookType := getHookType(hookName)

			logging.Debug(ctx, "hook invoked",
				slog.String("hook", hookName),
				slog.String("hook_type", hookType),
				slog.String("strategy", strategyName),
			)

			handler := GetHookHandler(agentName, hookName)
			if handler == nil {
				logging.Error(ctx, "no handler registered",
					slog.String("hook", hookName),
					slog.String("hook_type", hookType),
				)
				return fmt.Errorf("no handler registered for %s/%s", agentName, hookName)
			}

			// Set the current hook agent so handlers can retrieve it
			// without guessing from directory presence
			currentHookAgentName = agentName
			defer func() { currentHookAgentName = "" }()

			hookErr := handler()

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
