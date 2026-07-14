package cli

import (
	"fmt"
	"runtime"

	"github.com/entireio/cli/cmd/entire/cli/experimental"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

const gettingStarted = `

Getting Started:
  To get started with Entire CLI, run 'entire enable' to enable
  session tracking in your repository, then 'entire agent add <name>'
  to install hooks for a specific agent. For more information, visit:
  https://docs.entire.io/overview

`

const accessibilityHelp = `
Environment Variables:
  ACCESSIBLE    Set to any value (e.g., ACCESSIBLE=1) to enable accessibility
                mode. This uses simpler text prompts instead of interactive
                TUI elements, which works better with screen readers.
`

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "entire",
		Short:   "Entire CLI",
		Long:    "The command-line interface for Entire" + gettingStarted + accessibilityHelp,
		Version: versioninfo.Version,
		// Let main.go handle error printing to avoid duplication
		SilenceErrors: true,
		SilenceUsage:  true,
		// Hide completion command from help but keep it functional
		CompletionOptions: cobra.CompletionOptions{
			HiddenDefaultCmd: true,
		},
		PersistentPostRun: func(cmd *cobra.Command, _ []string) {
			// Skip for hidden commands (walk parent chain — Cobra doesn't propagate Hidden)
			for c := cmd; c != nil; c = c.Parent() {
				if c.Hidden {
					return
				}
			}

			// Load settings once for telemetry and version check
			var telemetryEnabled *bool
			settings, err := LoadEntireSettings(cmd.Context())
			if err == nil {
				telemetryEnabled = settings.Telemetry
			}

			// Check if telemetry is enabled
			if telemetryEnabled != nil && *telemetryEnabled {
				// Use detached tracking (non-blocking)
				installedAgents := GetAgentsWithHooksInstalled(cmd.Context())
				agentStr := JoinAgentNames(installedAgents)
				telemetry.TrackCommandDetached(cmd, agentStr, settings.Enabled, versioninfo.Version)
			}

			// Version check and notification (synchronous with 2s timeout)
			// Runs AFTER command completes to avoid interfering with interactive modes.
			// Stderr, never stdout: this hook also fires after --json commands whose
			// stdout is piped into jq or captured by scripts — a notice on stdout
			// corrupts that output while staying invisible in the caller's logs.
			versioncheck.CheckAndNotify(cmd.Context(), cmd.ErrOrStderr(), versioninfo.Version)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// If we're in a git repo but Entire isn't set up yet, start the setup flow
			if _, err := paths.WorktreeRoot(ctx); err == nil && !settings.IsSetUpAny(ctx) {
				return runSetupFlow(ctx, cmd.OutOrStdout(), EnableOptions{})
			}
			return cmd.Help()
		},
	}

	// Noun groups (canonical homes for subcommands).
	cmd.AddCommand(newSessionsCmd())                // 'session' (with 'sessions' as Cobra alias)
	cmd.AddCommand(newCheckpointGroupCmd())         // 'checkpoint' / 'cp' / 'checkpoints'
	experimental.Register(cmd, newTokensGroupCmd()) // 'tokens' (experimental)
	cmd.AddCommand(newAgentGroupCmd())              // 'agent'
	cmd.AddCommand(newAuthCmd())                    // 'auth'
	cmd.AddCommand(newDoctorCmd())                  // 'doctor' (group: trace/logs/bundle)
	cmd.AddCommand(newLabsCmd())                    // 'labs' (experimental workflow discovery)
	cmd.AddCommand(newPluginGroupCmd())             // 'plugin' (managed install/list/remove)
	experimental.Register(cmd, newImportCmd())      // 'import' (experimental; import pre-existing agent history)
	cmd.AddCommand(newOrgCmd())                     // 'org' — control-plane org management
	cmd.AddCommand(newProjectCmd())                 // 'project' — control-plane project management
	cmd.AddCommand(newRepoCmd())                    // 'repo' — control-plane repo lifecycle
	cmd.AddCommand(newGrantCmd())                   // 'grant' — control-plane access grants

	// Top-level lifecycle and standalone commands.
	experimental.Register(cmd, cliReview.NewCommand(buildReviewDeps()))        // `review` (experimental)
	experimental.Register(cmd, investigate.NewCommand(buildInvestigateDeps())) // `investigate` (experimental); multi-agent investigation
	cmd.AddCommand(newCleanCmd())
	cmd.AddCommand(newSetupCmd()) // 'configure' — non-agent settings; agent CRUD lives under 'agent'
	cmd.AddCommand(newEnableCmd())
	cmd.AddCommand(newDisableCmd())
	cmd.AddCommand(newStatusCmd())
	experimental.Register(cmd, newBlameCmd()) // 'blame' (experimental)
	experimental.Register(cmd, newWhyCmd())   // 'why' (experimental)
	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newLogoutCmd())
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newDispatchCmd())
	cmd.AddCommand(newActivityCmd())
	cmd.AddCommand(newRecapCmd())
	cmd.AddCommand(newAPICmd())          // authenticated passthrough to core/cell APIs
	cmd.AddCommand(newAgentHelpCmd(cmd)) // visible: agents on transports without context injection discover it via `entire help`

	// Hidden top-level shortcuts. Functional but print a deprecation hint.
	cmd.AddCommand(hideAsAlias(newResumeCmd(), "entire session resume"))
	cmd.AddCommand(hideAsAlias(newAttachCmd(), "entire session attach"))
	cmd.AddCommand(hideAsAlias(newExplainCmd(), "entire checkpoint explain"))
	cmd.AddCommand(hideAsAlias(newTraceCmd(), "entire doctor trace"))
	experimental.Register(cmd, newSearchCmd()) // 'entire search' = 'checkpoint search' (experimental)

	// Experimental labs commands (listed via `entire labs`; not deprecation shortcuts).
	experimental.Register(cmd, newExpertsCmd()) // 'experts' (experimental); agent/workflow provenance

	// Deprecated top-level commands (functional; the constructors mark them
	// Deprecated, which also excludes them from help and completion).
	cmd.AddCommand(newResetCmd())
	cmd.AddCommand(newRewindCmd())

	// Hidden infrastructure.
	cmd.AddCommand(newMCPCmd(cmd)) // MCP stdio server for MCP-host agents
	cmd.AddCommand(newHooksCmd())
	cmd.AddCommand(newTrailCmd())
	cmd.AddCommand(newSendAnalyticsCmd())
	cmd.AddCommand(newCurlBashPostInstallCmd())

	// Experimental command (developer-only visibility; setup/tune runners).
	experimental.Register(cmd, newRunnerCmd()) // 'runner' (experimental)

	cmd.SetVersionTemplate(versionString())

	// Replace default help command with custom one that supports -t flag
	cmd.SetHelpCommand(NewHelpCmd(cmd))

	return cmd
}

func versionString() string {
	return fmt.Sprintf("Entire CLI %s\nGo version: %s\nOS/Arch: %s/%s\n",
		versioninfo.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show build information",
		Run: func(cmd *cobra.Command, _ []string) {
			// Use OutOrStdout explicitly — cobra's cmd.Print() defaults to
			// stderr in v1.10+, but version output should go to stdout.
			fmt.Fprint(cmd.OutOrStdout(), versionString())
		},
	}
}

// newSendAnalyticsCmd creates the hidden command for sending analytics from a detached subprocess.
// This command is invoked by TrackCommandDetached and should not be called directly by users.
func newSendAnalyticsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__send_analytics",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			telemetry.SendEvent(args[0])
		},
	}
}
