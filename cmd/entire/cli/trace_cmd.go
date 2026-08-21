package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/perf"
	"github.com/spf13/cobra"
)

// summaryDefaultLast is the window --summary uses when --last was not given.
// Aggregating over the default of 1 trace would be useless, and asking every
// caller to remember a second flag is worse than picking a sensible window.
const summaryDefaultLast = 200

func newTraceCmd() *cobra.Command {
	var last int
	var hookFilter string
	var jsonOut bool
	var summary bool
	var slowOnly bool

	cmd := &cobra.Command{
		Use:   "trace",
		Short: "Show hook performance traces",
		// The threshold and env var come from perf rather than being written out
		// here: this help text already went stale once by describing the old
		// DEBUG-only behaviour, and a hardcoded copy is what let that happen.
		Long: fmt.Sprintf(`Show timing information for recent hook invocations.

Hooks that take %s or longer are traced by default, at WARN level, so a slow
hook records its own breakdown without any configuration. Set
%s=0 to turn that off; a log_level above WARN also hides them.

To trace every hook, including fast ones, raise verbosity instead:
  - Set ENTIRE_LOG_LEVEL=DEBUG in your shell profile
  - Add "log_level": "DEBUG" to .entire/settings.json

Examples:
  entire doctor trace                     Show the most recent hook trace
  entire doctor trace --last 5            Show the last 5 hook traces
  entire doctor trace --hook post-commit  Show only post-commit hook traces
  entire doctor trace --summary           Aggregate: which step dominates, per hook
  entire doctor trace --summary --slow    Aggregate only the slow traces
  entire doctor trace --last 20 --json    Machine-readable output`,
			perf.DefaultSlowSpanThreshold, perf.SlowSpanEnvVar),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if last < 1 {
				return fmt.Errorf("--last must be at least 1, got %d", last)
			}
			if jsonOut && summary {
				return errors.New("--json and --summary are mutually exclusive")
			}
			if summary && !cmd.Flags().Changed("last") {
				last = summaryDefaultLast
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

			entries, err := collectTraceEntries(logFile, last, hookFilter, slowOnly)
			if err != nil {
				return fmt.Errorf("collecting trace entries: %w", err)
			}

			switch {
			case jsonOut:
				return renderTraceJSON(cmd.OutOrStdout(), entries)
			case summary:
				renderTraceSummary(cmd.OutOrStdout(), summarizeTraces(entries))
			default:
				renderTraceEntries(cmd.OutOrStdout(), entries)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&last, "last", 1, "Show last N hook invocations")
	cmd.Flags().StringVar(&hookFilter, "hook", "", "Filter by hook type (e.g. post-commit, prepare-commit-msg, pre-push)")
	cmd.Flags().BoolVar(&summary, "summary", false,
		fmt.Sprintf("Aggregate across traces instead of showing each one (uses --last %d unless given)", summaryDefaultLast))
	cmd.Flags().BoolVar(&slowOnly, "slow", false, "Only include traces flagged slow (those logged at WARN)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")

	return cmd
}
