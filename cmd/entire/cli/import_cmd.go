package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "import",
		Short:  "Import pre-existing agent history into Entire (experimental)",
		Hidden: true,
		RunE:   func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	// One subcommand per registered importer, so adding an agent is just a new
	// agentimport.Importer registration — no command wiring needed here.
	for _, imp := range agentimport.All() {
		cmd.AddCommand(newImportAgentCmd(imp))
	}
	return cmd
}

func newImportAgentCmd(imp agentimport.Importer) *cobra.Command {
	var pathFlag string
	var dryRun bool
	var sessions []string

	cmd := &cobra.Command{
		Use:   imp.Name(),
		Short: fmt.Sprintf("Import existing %s transcripts as read-only checkpoints", imp.AgentType()),
		Long: fmt.Sprintf(`Import pre-existing %s transcripts for this repo (the past month) as
read-only checkpoints. Imported history is searchable and explainable, but
read-only: imported sessions cannot be resumed.

Import honors checkpoint policy before scanning transcripts. If the configured
checkpoint_version or checkpoint_min_version is unsupported by this CLI, import
fails even with --dry-run.`, imp.AgentType()),
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			repoRoot, err := paths.WorktreeRoot(ctx)
			if err != nil {
				c.SilenceUsage = true
				fmt.Fprintln(c.ErrOrStderr(), "Not a git repository. Run 'entire enable' from within a git repository.")
				return NewSilentError(err)
			}
			repo, err := openRepository(ctx)
			if err != nil {
				return fmt.Errorf("open repository: %w", err)
			}
			defer repo.Close()

			// Best-effort file logging (like explain/resume): without Init,
			// logging.Debug below is a no-op. WorktreeRoot already succeeded,
			// so this cannot create .entire/logs/ outside a repo.
			logging.SetLogLevelGetter(GetLogLevel)
			if err := logging.Init(ctx, ""); err == nil {
				defer logging.Close()
			}

			if err := ensureCheckpointPolicyAllowsCheckpointData(ctx, repo); err != nil {
				return err
			}

			// Load repo/user-configured redaction (opt-in PII, custom_redactions,
			// redactor packs) before any checkpoint write. Imported transcripts
			// are redacted with redact.JSONLBytes, which honors this config; without
			// it only always-on secret scanning would run on imported history.
			strategy.EnsureRedactionConfigured()

			// Logged so support can tell why an import has no anchor (empty
			// sha: nothing resolved) or a stale one (origin tip not fetched).
			linkCommitSHA := resolveImportLinkCommitSHA(repo)
			logging.Debug(ctx, "import: resolved link commit", "commit_sha", linkCommitSHA)

			progress, stopProgress := newImportProgressReporter(c.OutOrStdout(), string(imp.AgentType()))
			res, err := agentimport.Run(ctx, repo, imp, agentimport.Options{
				RepoRoot: repoRoot, OverridePath: pathFlag, SessionFilter: sessions,
				Now: time.Now(), DryRun: dryRun,
				LinkCommitSHA: linkCommitSHA,
				Progress:      progress,
			})
			stopProgress(err == nil)
			if err != nil {
				// Ctrl-C is not a failure: report the partial import (turns
				// already written stay written, and a re-run resumes where
				// this one stopped) instead of a raw "context canceled".
				if errors.Is(err, context.Canceled) {
					c.SilenceUsage = true
					fmt.Fprintf(c.OutOrStdout(), "Import interrupted after %d turn(s). Re-run to finish.\n", res.TurnsImported)
					return NewSilentError(err)
				}
				return fmt.Errorf("import %s: %w", imp.Name(), err)
			}
			verb := "Imported"
			if dryRun {
				verb = "Would import"
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %d turn(s) from %d session(s) (%d already imported).\n",
				verb, res.TurnsImported, res.SessionsScanned, res.TurnsSkipped)
			// A dry run writes nothing locally, so there is nothing to sync.
			if !dryRun {
				warnIfImportNotSynced(c.OutOrStdout(), res.TurnsImported > 0 || res.TurnsSkipped > 0)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pathFlag, "path", "", "Override the transcript directory to import from")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be imported without writing")
	cmd.Flags().StringSliceVar(&sessions, "session", nil, "Import only these session IDs (repeatable)")
	return cmd
}
