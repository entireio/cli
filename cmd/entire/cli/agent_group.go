package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/spf13/cobra"
)

// newAgentGroupCmd builds `entire agent`. Replaces `entire configure`.
func newAgentGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agent integrations (add, remove, list)",
		Long: `Manage agent integrations in this repository.

Commands:
  list     Show installed and available agents
  add      Install hooks for an agent
  remove   Uninstall hooks for an agent

Examples:
  entire agent
  entire agent list
  entire agent add claude-code
  entire agent remove claude-code`,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := paths.WorktreeRoot(cmd.Context()); err != nil {
				return errors.New("not a git repository")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentMenu(cmd.Context(), cmd.OutOrStdout())
		},
	}

	cmd.AddCommand(newAgentListCmd())
	cmd.AddCommand(newAgentAddCmd())
	cmd.AddCommand(newAgentRemoveCmd())
	return cmd
}

func runAgentMenu(ctx context.Context, w io.Writer) error {
	opts := EnableOptions{Telemetry: true}
	if settings.IsSetUpAny(ctx) {
		return runManageAgents(ctx, w, opts, nil)
	}
	return runSetupFlow(ctx, w, opts)
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed and available agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentList(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runAgentList(ctx context.Context, w io.Writer) error {
	installed := GetAgentsWithHooksInstalled(ctx)
	installedSet := make(map[types.AgentName]struct{}, len(installed))
	for _, name := range installed {
		installedSet[name] = struct{}{}
	}

	all := agent.StringList()

	fmt.Fprintln(w, "Agents:")
	for _, name := range all {
		marker := "  "
		if _, ok := installedSet[types.AgentName(name)]; ok {
			marker = "✓ "
		}
		fmt.Fprintf(w, "  %s%s\n", marker, name)
	}
	if len(installed) == 0 {
		fmt.Fprintln(w, "\nNo agents installed. Use 'entire agent add <name>' to install hooks.")
	}
	return nil
}

func newAgentAddCmd() *cobra.Command {
	var localDev bool
	var forceHooks bool

	cmd := &cobra.Command{
		Use:   "add <agent-name>",
		Short: "Install hooks for an agent",
		Long: `Install hooks for the specified agent in this repository.

Examples:
  entire agent add claude-code
  entire agent add gemini-cli`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ag, err := agent.Get(types.AgentName(name))
			if err != nil {
				printWrongAgentError(cmd.OutOrStdout(), name)
				return NewSilentError(errors.New("wrong agent name"))
			}
			opts := EnableOptions{
				LocalDev:   localDev,
				ForceHooks: forceHooks,
				Telemetry:  true,
			}
			return setupAgentHooksNonInteractive(cmd.Context(), cmd.OutOrStdout(), ag, opts)
		},
	}

	cmd.Flags().BoolVar(&localDev, "local-dev", false, "Install hooks in local-dev mode")
	cmd.Flags().BoolVar(&forceHooks, "force", false, "Reinstall hooks even if already present")
	return cmd
}

func newAgentRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <agent-name>",
		Short: "Uninstall hooks for an agent",
		Long: `Uninstall hooks for the specified agent in this repository.

Examples:
  entire agent remove claude-code`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemoveAgent(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}
