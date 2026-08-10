package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
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
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed and available agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentList(cmd.Context(), cmd.OutOrStdout(), all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false,
		"Also list available external agent plugins found on your PATH, not just installed ones")
	return cmd
}

func runAgentList(ctx context.Context, w io.Writer, all bool) error {
	// Discover external agent plugins so installed ones appear in the listing.
	// Default: gated discovery (honors external_agents). An external agent can
	// only become installed via `agent add`, which enables external_agents, so
	// this surfaces every installed external without executing plugins for users
	// who never opted in. `--all` uses ungated discovery so available-but-
	// uninstalled plugins on $PATH are found and shown too.
	if all {
		external.DiscoverAndRegisterAlways(ctx)
	} else {
		external.DiscoverAndRegister(ctx)
	}

	installed := GetAgentsWithHooksInstalled(ctx)
	installedSet := make(map[types.AgentName]struct{}, len(installed))
	for _, name := range installed {
		installedSet[name] = struct{}{}
	}

	fmt.Fprintln(w, "Agents:")
	for _, name := range agent.StringList() {
		agentName := types.AgentName(name)
		_, isInstalled := installedSet[agentName]

		// Built-ins and installed externals always appear. Available (not-yet-
		// installed) external plugins appear only with --all.
		if !all && !isInstalled {
			if ag, err := agent.Get(agentName); err == nil && external.IsExternal(ag) {
				continue
			}
		}

		marker := "  "
		if isInstalled {
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
	var searchSkill bool
	var agentHelpSkill bool

	cmd := &cobra.Command{
		Use:   "add <agent-name>",
		Short: "Install hooks for an agent",
		Long: `Install hooks for the specified agent in this repository.

Examples:
  entire agent add claude-code
  entire agent add gemini`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Resolve the named external agent plugin so `add` works like
			// `entire enable --agent <name>`. Targeting the single name (rather
			// than a full $PATH scan) avoids executing every unrelated
			// entire-agent-* binary and surfaces a real load error instead of a
			// generic "unknown agent". Bypasses the setting gate because this
			// command is itself the explicit opt-in.
			if err := external.DiscoverAndRegisterNamedAlways(cmd.Context(), types.AgentName(name)); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("loading external agent %q: %w", name, err)
			}
			ag, err := agent.Get(types.AgentName(name))
			if err != nil {
				printWrongAgentError(cmd.OutOrStdout(), name)
				return NewSilentError(errors.New("wrong agent name"))
			}
			opts := EnableOptions{
				LocalDev:       localDev,
				ForceHooks:     forceHooks,
				SearchSkill:    searchSkill,
				AgentHelpSkill: agentHelpSkill,
				Telemetry:      true,
			}
			return setupAgentHooksNonInteractive(cmd.Context(), cmd.OutOrStdout(), ag, opts)
		},
	}

	cmd.Flags().BoolVar(&localDev, "local-dev", false, "Install hooks in local-dev mode")
	cmd.Flags().BoolVar(&forceHooks, "force", false, "Reinstall hooks even if already present")
	cmd.Flags().BoolVar(&searchSkill, flagSearchSkill, false, "Install the optional Entire search skill")
	cmd.Flags().BoolVar(&agentHelpSkill, flagAgentHelpSkill, false, "Install the stable Entire agent-help skill (points agents at `entire agent-help`)")
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
			// Resolve the named external agent plugin so `remove` can find it,
			// executing only that binary rather than scanning all of $PATH.
			if err := external.DiscoverAndRegisterNamedAlways(cmd.Context(), types.AgentName(args[0])); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("loading external agent %q: %w", args[0], err)
			}
			return runRemoveAgent(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}
