// Package ticket implements `entire ticket`, the platform-agnostic surface for
// linking work in a repository to tickets on external trackers such as Linear
// and Jira.
//
// The command layer depends only on the canonical Task type and the Provider
// interface; concrete trackers implement Provider in their own files so the
// surface never depends on a specific platform.
package ticket

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// NewCommand returns the `entire ticket` cobra group. It is hidden from
// `entire help` while the feature matures; direct invocation still works.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ticket",
		Short: "Link work to tickets on Linear, Jira, and other platforms",
		// Hidden from `entire help` while the feature is still maturing;
		// directly invoking it still works.
		Hidden: true,
		Long: `Link work in this repository to tickets on an external tracker such as
Linear or Jira, so agents and reviews carry the ticket as grounding context.`,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := paths.WorktreeRoot(cmd.Context()); err != nil {
				return errors.New("not a git repository")
			}
			// Best-effort: route ticket-command debug logs to .entire/logs/
			// (matches how other standalone commands initialize logging). A
			// failure just means no file logging; the command still runs.
			logging.Init(cmd.Context(), "") //nolint:errcheck,gosec // best-effort logging init
			return nil
		},
		// Flush the buffered log on the way out. Hidden commands skip root's
		// PersistentPostRun, so the group flushes its own logs.
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			logging.Close()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newSetupCmd())
	cmd.AddCommand(newLinkCmd())
	cmd.AddCommand(newUnlinkCmd())

	return cmd
}
