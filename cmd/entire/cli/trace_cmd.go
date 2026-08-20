package cli

import (
	"fmt"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

func newTraceCmd() *cobra.Command {
	var last int
	var hookFilter string

	cmd := &cobra.Command{
		Use:   "trace",
		Short: "Show hook performance traces",
		Long: `Show timing information for recent hook invocations.

Traces are emitted at DEBUG log level. To enable them, either:
  - Set ENTIRE_LOG_LEVEL=DEBUG in your shell profile
  - Add "log_level": "DEBUG" to .entire/settings.json

Examples:
  entire doctor trace                     Show the most recent hook trace
  entire doctor trace --last 5            Show the last 5 hook traces
  entire doctor trace --hook post-commit  Show only post-commit hook traces`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if last < 1 {
				return fmt.Errorf("--last must be at least 1, got %d", last)
			}

			// AbsPath (not a bare WorktreeRoot join): globally tracked
			// repos route .entire/logs under the git common dir.
			logFile, err := paths.AbsPath(cmd.Context(), filepath.Join(logging.LogsDir, "entire.log"))
			if err != nil {
				cmd.SilenceUsage = true
				// A routing failure happens inside a valid git repo —
				// "not a git repository" would misdiagnose it.
				if paths.IsUnroutableRuntimePath(err) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Logs unavailable: %v.\n", err)
					return NewSilentError(err)
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
				return NewSilentError(fmt.Errorf("not a git repository: %w", err))
			}

			entries, err := collectTraceEntries(logFile, last, hookFilter)
			if err != nil {
				return fmt.Errorf("collecting trace entries: %w", err)
			}

			renderTraceEntries(cmd.OutOrStdout(), entries)
			return nil
		},
	}

	cmd.Flags().IntVar(&last, "last", 1, "Show last N hook invocations")
	cmd.Flags().StringVar(&hookFilter, "hook", "", "Filter by hook type (e.g. post-commit, prepare-commit-msg, pre-push)")

	return cmd
}
