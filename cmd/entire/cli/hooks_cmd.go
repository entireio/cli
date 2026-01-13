package cli

import (
	"entire.io/cli/cmd/entire/cli/agent"
	// Import agents to ensure they are registered before we iterate
	_ "entire.io/cli/cmd/entire/cli/agent/claudecode"
	_ "entire.io/cli/cmd/entire/cli/agent/geminicli"

	"github.com/spf13/cobra"
)

func newHooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hooks",
		Short:  "Hook handlers",
		Long:   "Commands called by hooks. These are internal and not for direct user use.",
		Hidden: true, // Internal command, not for direct user use
	}

	// Git hooks are strategy-level (not agent-specific)
	cmd.AddCommand(newHooksGitCmd())

	// Dynamically add agent hook subcommands
	// Each agent that implements HookHandler gets its own subcommand tree
	for _, agentName := range agent.List() {
		ag, err := agent.Get(agentName)
		if err != nil {
			continue
		}
		if handler, ok := ag.(agent.HookHandler); ok {
			cmd.AddCommand(newAgentHooksCmd(agentName, handler))
		}
	}

	return cmd
}
