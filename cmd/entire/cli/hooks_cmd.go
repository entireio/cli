package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	// Import agents to ensure they are registered before we iterate
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/vogon"

	// support external agents
	"github.com/entireio/cli/cmd/entire/cli/agent/external"

	"github.com/spf13/cobra"
)

func newHooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hooks",
		Short:  "Hook handlers",
		Long:   "Commands called by hooks. These are internal and not for direct user use.",
		Hidden: true, // Internal command, not for direct user use
		// RunE handles external agent hooks that aren't registered as subcommands.
		// When Cobra can't match a subcommand (e.g., "entire hooks my-ext-agent stop"),
		// it falls back to this RunE with args ["my-ext-agent", "stop"].
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return cmd.Help()
			}
			agentName := types.AgentName(args[0])
			hookName := args[1]

			// Lazily discover external agents only when actually needed.
			discoveryCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			external.DiscoverAndRegister(discoveryCtx)

			// Verify the agent was discovered
			if _, err := agent.Get(agentName); err != nil {
				return fmt.Errorf("unknown agent %q (not found as built-in or external plugin)", agentName)
			}

			return executeAgentHook(cmd, agentName, hookName, true)
		},
	}

	// Git hooks are strategy-level (not agent-specific)
	cmd.AddCommand(newHooksGitCmd())

	// Only built-in agents are registered eagerly (no process spawning).
	// External agents are discovered lazily via the RunE fallback above.
	for _, agentName := range agent.List() {
		ag, err := agent.Get(agentName)
		if err != nil {
			continue
		}
		if handler, ok := agent.AsHookSupport(ag); ok {
			cmd.AddCommand(newAgentHooksCmd(agentName, handler))
		}
	}

	return cmd
}
