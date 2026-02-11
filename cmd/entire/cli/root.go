package cli

import (
	"fmt"
	"runtime"

	"github.com/entireio/cli/cmd/entire/cli/buildinfo"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/spf13/cobra"
)

const gettingStarted = `

Getting Started:
  To get started with Entire CLI, run 'entire enable' to configure
  your project's environment. For more information, visit:
  https://docs.entire.io/introduction

`

const accessibilityHelp = `
Environment Variables:
  ACCESSIBLE    Set to any value (e.g., ACCESSIBLE=1) to enable accessibility
                mode. This uses simpler text prompts instead of interactive
                TUI elements, which works better with screen readers.
`

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entire",
		Short: "Entire CLI",
		Long:  "The command-line interface for Entire" + gettingStarted + accessibilityHelp,
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
			settings, err := LoadEntireSettings()
			if err == nil {
				telemetryEnabled = settings.Telemetry
			}

			// Check if telemetry is enabled
			if telemetryEnabled != nil && *telemetryEnabled {
				// Use detached tracking (non-blocking)
				installedAgents := GetAgentsWithHooksInstalled()
				agentStr := JoinAgentNames(installedAgents)
				telemetry.TrackCommandDetached(cmd, settings.Strategy, agentStr, settings.Enabled, buildinfo.Version)
			}

			// Version check and notification (synchronous with 2s timeout)
			// Runs AFTER command completes to avoid interfering with interactive modes
			versioncheck.CheckAndNotify(cmd.OutOrStdout(), buildinfo.Version)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	// Add subcommands here
	cmd.AddCommand(newRewindCmd())
	cmd.AddCommand(newResumeCmd())
	cmd.AddCommand(newCleanCmd())
	cmd.AddCommand(newResetCmd())
	cmd.AddCommand(newEnableCmd())
	cmd.AddCommand(newDisableCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newHooksCmd())
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newExplainCmd())
	cmd.AddCommand(newDebugCmd())
	cmd.AddCommand(newDoctorCmd())
	cmd.AddCommand(newCodexCmd())
	cmd.AddCommand(newSendAnalyticsCmd())
	cmd.AddCommand(newCurlBashPostInstallCmd())

	// Replace default help command with custom one that supports -t flag
	cmd.SetHelpCommand(NewHelpCmd(cmd))

	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show build information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("Entire CLI %s (%s)\n", buildinfo.Version, buildinfo.Commit)
			fmt.Printf("Go version: %s\n", runtime.Version())
			fmt.Printf("OS/Arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
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
