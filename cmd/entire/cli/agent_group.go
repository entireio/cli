package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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
	var externalOnly bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List built-in agents (use --external to list external plugins on your PATH)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentList(cmd.Context(), cmd.OutOrStdout(), externalOnly)
		},
	}
	cmd.Flags().BoolVar(&externalOnly, "external", false,
		"List external agent plugins found on your PATH instead of built-in agents")
	return cmd
}

func runAgentList(ctx context.Context, w io.Writer, externalOnly bool) error {
	// --external is the explicit opt-in to the costly path: register external
	// plugins from $PATH so they list through the same rendering as built-ins
	// (real names, capabilities, test-only filtering). The default path never
	// discovers, so it stays fast regardless of how many plugins are on $PATH.
	if externalOnly {
		external.DiscoverAndRegisterAlways(ctx)
	}

	hasSomeInstalled := false
	fmt.Fprintln(w, "Agents:")
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			logging.Debug(ctx, "agent registered but unresolvable, skipping",
				slog.String("agent", string(name)), slog.String("error", err.Error()))
			continue
		}
		if to, ok := ag.(agent.TestOnly); ok && to.IsTestOnly() {
			continue
		}
		// Default lists built-in agents; --external lists external plugins.
		if external.IsExternal(ag) != externalOnly {
			continue
		}
		installed := false
		if hs, ok := agent.AsHookSupport(ag); ok && hs.AreHooksInstalled(ctx) {
			installed = true
			hasSomeInstalled = true
		}
		marker := "  "
		if installed {
			marker = "✓ "
		}
		fmt.Fprintf(w, "  %s%s\n", marker, name)
	}

	if !hasSomeInstalled {
		fmt.Fprintln(w, "\nNo agents installed. Use 'entire agent add <name>' to install hooks.")
	}
	if !externalOnly {
		fmt.Fprintln(w, "\nRun 'entire agent list --external' to list external agent plugins on your PATH.")
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
