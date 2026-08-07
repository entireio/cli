package cli

import (
	"fmt"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
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
	var reconcile bool
	var acceptHeuristics bool
	var lookbackDays int
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   imp.Name(),
		Short: fmt.Sprintf("Import existing %s transcripts as read-only checkpoints", imp.AgentType()),
		Long: fmt.Sprintf(`Import pre-existing %s transcripts for this repo (the past month by
default, see --lookback) as read-only checkpoints. Imported history is
searchable and explainable but is not rewindable.

With --reconcile, import also looks for commits in the same window that have no
session data at all — no Entire-Checkpoint trailer — and links the imported
turns to them. Existing commits are never rewritten: the link is stored on the
checkpoint, along with how it was derived.

  recorded   the turn's own transcript recorded making that commit
  heuristic  the turn was matched to the commit by time window, 1:1 and
             unambiguous. Reported as a candidate; --accept-heuristics writes it
  fallback   no link — the default-branch tip, shown as a display anchor only

Reconcile also backfills links onto checkpoints an earlier import already
wrote, and never weakens an existing "recorded" link. Re-running is a no-op.

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

			// --accept-heuristics implies --reconcile: accepting matches from a
			// scan that never runs would silently do nothing.
			reconciling := reconcile || acceptHeuristics
			var reconcileOpts *agentimport.ReconcileOptions
			var scanTips []plumbing.Hash
			if reconciling {
				reconcileOpts = &agentimport.ReconcileOptions{Enabled: true, AcceptHeuristics: acceptHeuristics}
				scanTips = resolveImportScanTips(repo)
				logging.Debug(ctx, "import: resolved reconcile scan tips", "tips", len(scanTips))
			}

			// --json owns stdout: a progress bar interleaved with the document
			// would make it unparseable.
			var progress *agentimport.Progress
			stopProgress := func(bool) {}
			if !jsonOut {
				progress, stopProgress = newImportProgressReporter(c.OutOrStdout(), string(imp.AgentType()))
			}
			res, err := agentimport.Run(ctx, repo, imp, agentimport.Options{
				RepoRoot: repoRoot, OverridePath: pathFlag, SessionFilter: sessions,
				Now: time.Now(), DryRun: dryRun,
				LookbackDays:  lookbackDays,
				LinkCommitSHA: linkCommitSHA,
				Reconcile:     reconcileOpts,
				ScanTips:      scanTips,
				Progress:      progress,
			})
			stopProgress(err == nil)
			if err != nil {
				return fmt.Errorf("import %s: %w", imp.Name(), err)
			}
			if jsonOut {
				return writeImportJSON(c.OutOrStdout(), imp.Name(), res, dryRun)
			}
			verb := "Imported"
			if dryRun {
				verb = "Would import"
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %d turn(s) from %d session(s) (%d already imported).\n",
				verb, res.TurnsImported, res.SessionsScanned, res.TurnsSkipped)
			writeReconcileReport(c.OutOrStdout(), res.Report)
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
	cmd.Flags().BoolVar(&reconcile, "reconcile", false,
		"Also link imported turns to commits that have no session data, and report the rest")
	cmd.Flags().BoolVar(&acceptHeuristics, "accept-heuristics", false,
		"Write time-window matches as links instead of only reporting them (implies --reconcile)")
	cmd.Flags().IntVar(&lookbackDays, "lookback", agentimport.DefaultLookbackDays,
		"How many days back to scan for transcripts and commits")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the result as JSON (suppresses progress output)")
	return cmd
}
