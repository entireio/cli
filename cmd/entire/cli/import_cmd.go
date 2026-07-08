package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
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
read-only checkpoints. Imported history is searchable and explainable but is
not rewindable.

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

			if err := ensureCheckpointPolicyAllowsCheckpointData(ctx, repo); err != nil {
				return err
			}

			// Load repo/user-configured redaction (opt-in PII, custom_redactions,
			// redactor packs) before any checkpoint write. Imported transcripts
			// are redacted with redact.JSONLBytes, which honors this config; without
			// it only always-on secret scanning would run on imported history.
			strategy.EnsureRedactionConfigured()

			res, err := agentimport.Run(ctx, repo, imp, agentimport.Options{
				RepoRoot: repoRoot, OverridePath: pathFlag, SessionFilter: sessions,
				Now: time.Now(), DryRun: dryRun,
			})
			if err != nil {
				return fmt.Errorf("import %s: %w", imp.Name(), err)
			}
			verb := "Imported"
			if dryRun {
				verb = "Would import"
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %d turn(s) from %d session(s) (%d already imported).\n",
				verb, res.TurnsImported, res.SessionsScanned, res.TurnsSkipped)
			return nil
		},
	}
	cmd.Flags().StringVar(&pathFlag, "path", "", "Override the transcript directory to import from")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be imported without writing")
	cmd.Flags().StringSliceVar(&sessions, "session", nil, "Import only these session IDs (repeatable)")
	return cmd
}
