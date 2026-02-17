package cli

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/dashboard"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Open interactive session dashboard",
		Long: `Interactive TUI dashboard for browsing sessions, checkpoints,
active sessions, and settings.

Navigate with Tab/Shift+Tab between tabs, j/k or arrow keys to move,
Enter to view details, and q to quit.

In the Checkpoints tab, press r to rewind to a selected checkpoint.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if we're in a git repository
			if _, err := paths.RepoRoot(); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			// Check if Entire is disabled
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			// Accessible mode uses text-based menu
			if IsAccessibleMode() {
				return dashboard.RunAccessible(cmd.OutOrStdout())
			}

			// Run TUI dashboard
			rewindReq, err := dashboard.Run()
			if err != nil {
				return fmt.Errorf("dashboard error: %w", err)
			}

			// Handle rewind request from dashboard
			if rewindReq != nil {
				return performRewindFromDashboard(cmd, rewindReq.PointID)
			}

			return nil
		},
	}
}

// performRewindFromDashboard executes a rewind after the dashboard TUI exits.
func performRewindFromDashboard(cmd *cobra.Command, pointID string) error {
	strat := GetStrategy()

	// Check if rewind is possible
	canRewind, reason, err := strat.CanRewind()
	if err != nil {
		return fmt.Errorf("failed to check rewind status: %w", err)
	}
	if !canRewind {
		fmt.Fprintln(cmd.OutOrStdout(), reason)
		return nil
	}

	// Find the matching rewind point
	points, err := strat.GetRewindPoints(100)
	if err != nil {
		return fmt.Errorf("failed to get rewind points: %w", err)
	}

	var target *strategy.RewindPoint
	for i := range points {
		if points[i].ID == pointID {
			target = &points[i]
			break
		}
	}

	if target == nil {
		return fmt.Errorf("rewind point %s not found", pointID)
	}

	// Preview rewind to show warnings about files that will be deleted
	preview, previewErr := strat.PreviewRewind(*target)
	if previewErr == nil && preview != nil && len(preview.FilesToDelete) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "\nWarning: The following untracked files will be DELETED:\n")
		for _, f := range preview.FilesToDelete {
			fmt.Fprintf(cmd.ErrOrStderr(), "  - %s\n", f)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\n")
	}

	// Perform the rewind
	if err := strat.Rewind(*target); err != nil {
		return fmt.Errorf("rewind failed: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Successfully rewound to checkpoint.")

	// Print resume command if agent info available
	if target.Agent != "" {
		resumeCmd := formatResumeCommand(target.Agent, target.SessionID)
		if resumeCmd != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\nTo resume: %s\n", resumeCmd)
		}
	}

	return nil
}

// formatResumeCommand generates the resume command for an agent.
func formatResumeCommand(agentType agent.AgentType, sessionID string) string {
	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		return ""
	}
	return ag.FormatResumeCommand(sessionID)
}
