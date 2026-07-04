package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func newDoctorRewriteCheckpointsCmd() *cobra.Command {
	var dryRun bool
	var force bool

	cmd := &cobra.Command{
		Use:   "rewrite-checkpoints",
		Short: "Re-materialize git-branch checkpoints as per-checkpoint git refs (test tooling)",
		Long: `Rewrite the checkpoints stored on the entire/checkpoints/v1 branch into
per-checkpoint refs under refs/entire/checkpoints/<shard>/<id>, the layout the
git-refs checkpoint store uses.

Each checkpoint is written fresh through the git-refs store: the transcript is
replayed (regenerating the compact transcript.jsonl), everything is rooted at
the checkpoint (no shard folders), and per-session summaries and combined
attribution are replayed. The checkpoint id is preserved and each commit keeps
the original author. Any subagent tasks/ subtree is grafted in unchanged.

This is test tooling for evaluating the git-refs store against real branch data:
it writes refs locally and does not push them. Idempotent — checkpoints that
already have a ref are skipped (use --force to re-materialize from scratch).`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			repo, err := strategy.OpenRepository(ctx)
			if err != nil {
				return fmt.Errorf("open repository: %w", err)
			}
			defer repo.Close()

			result, err := checkpoint.RewriteBranchToRefs(ctx, repo, dryRun, force)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return NewSilentError(err)
				}
				return fmt.Errorf("rewrite checkpoints: %w", err)
			}

			if result.Total == 0 {
				fmt.Fprintln(out, "No checkpoints found on the v1 branch — nothing to rewrite.")
				return nil
			}

			verb := "Rewrote"
			if dryRun {
				verb = "Would rewrite"
			}
			fmt.Fprintf(out, "%s %d checkpoint(s) to refs (%d already present, %d total).\n",
				verb, len(result.Rewritten), result.Skipped, result.Total)
			if !dryRun && len(result.Rewritten) > 0 {
				fmt.Fprintln(out, "Refs written locally under refs/entire/checkpoints/ — not pushed.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be rewritten without writing refs")
	cmd.Flags().BoolVar(&force, "force", false, "Re-materialize checkpoints whose ref already exists")
	return cmd
}
