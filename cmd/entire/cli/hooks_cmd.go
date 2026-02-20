package cli

import (
	"github.com/entireio/cli/cmd/entire/cli/agent"
	// Import agents to ensure they are registered before we iterate
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/opencode"

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
	for _, agentName := range agent.List() {
		ag, err := agent.Get(agentName)
		if err != nil {
			continue
		}
		cmd.AddCommand(newAgentHooksCmd(agentName, ag))
	}

	return cmd
}
