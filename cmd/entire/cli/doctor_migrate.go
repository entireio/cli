package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	git "github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func newDoctorMigrateCheckpointsCmd() *cobra.Command {
	var dryRun bool
	var remote string

	cmd := &cobra.Command{
		Use:   "migrate-checkpoints",
		Short: "Convert git-branch checkpoints into per-checkpoint git refs (git-refs store)",
		Long: `Convert the checkpoints stored on the entire/checkpoints/v1 branch into
per-checkpoint refs under refs/entire/checkpoints/<shard>/<id>, the layout the
git-refs checkpoint store uses.

Each checkpoint's current tree is wrapped in a fresh commit and its ref is
pointed at it — existing branch commits are not remapped. The checkpoint's
metadata is normalized for the new layout: the legacy checkpoint_version field
is dropped and session file paths are rewritten relative to the ref. The
command is idempotent: checkpoints already converted are skipped, so it is safe
to re-run after more branch activity.

New refs are queued for push. Run interactively, it asks whether to push them
now; non-interactively it never pushes — the refs stay queued and flush on the
next push once the git-refs store is the configured primary.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			// Once git-refs is the primary store the refs are authoritative and
			// the v1 branch may lag behind them; re-importing its snapshots
			// could only regress refs, so refuse.
			if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft: a bad checkpoints block already surfaces via Open; default to allowing migration
				fmt.Fprintln(out, "The git-refs store is already the primary checkpoint store — nothing to migrate.")
				return nil
			}

			repo, err := strategy.OpenRepository(ctx)
			if err != nil {
				return fmt.Errorf("open repository: %w", err)
			}
			defer repo.Close()

			result, err := checkpoint.MigrateBranchToRefs(ctx, repo, dryRun)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return NewSilentError(err)
				}
				return fmt.Errorf("migrate checkpoints: %w", err)
			}

			if result.Total == 0 {
				fmt.Fprintln(out, "No checkpoints found on the v1 branch — nothing to migrate.")
				return nil
			}

			verb := "Migrated"
			if dryRun {
				verb = "Would migrate"
			}
			fmt.Fprintf(out, "%s %d checkpoint(s) to refs (%d already up to date, %d total).\n",
				verb, len(result.Migrated), result.Skipped, result.Total)

			if dryRun || len(result.Migrated) == 0 {
				return nil
			}

			if !interactive.CanPromptInteractively() {
				fmt.Fprintln(out, "Refs are queued; they push on the next `git push` once git-refs is the primary store.")
				return nil
			}

			pushRemote, err := resolveMigratePushRemote(ctx, remote)
			if err != nil {
				return err
			}

			title := fmt.Sprintf("Push %d migrated checkpoint ref(s) now?", len(result.Migrated))
			confirmed, err := confirmDoctorFix(ctx, out, title)
			if err != nil {
				return err
			}
			if !confirmed {
				fmt.Fprintln(out, "Refs stay queued for the next push.")
				return nil
			}

			return pushMigratedRefs(ctx, out, repo, pushRemote)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be migrated without writing refs")
	cmd.Flags().StringVar(&remote, "remote", "", "Remote to push migrated refs to (default: the checkpoint sync remote)")
	return cmd
}

// pushMigratedRefs configures redaction, flushes the queued checkpoint refs and
// reports the outcome.
//
// It owns the redaction step rather than a PreRunE because the OPF gate inside
// the push reads process-global config that only EnsureRedactionConfigured
// sets, and an unconfigured one reads as "OPF off" and waves un-OPF'd
// checkpoint content through. Doing it here rather than up-front keeps the
// precondition on the one path that has it: --dry-run, an already-primary
// store, and a run with nothing migrated all return above without ever
// consulting redaction settings, so they must not fail on them. It also means
// the guarantee does not depend on where a parent command happens to configure
// redaction — cobra does not inherit PreRunE, and doctor's lives on its own.
func pushMigratedRefs(ctx context.Context, out io.Writer, repo *git.Repository, pushRemote string) error {
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		return fmt.Errorf("configure redaction before pushing: %w", err)
	}

	pushed, pushDisabled, err := strategy.PushQueuedCheckpointRefs(ctx, repo, pushRemote)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return NewSilentError(err)
		}
		// Ctrl-C at the OPF prompt is the same gesture as declining the push
		// prompt, and lands in the same place: nothing shipped, refs still
		// queued. confirmDoctorFix reports that as a clean decline, so this
		// must not report it as a failure.
		if errors.Is(err, strategy.ErrOPFAbortedByUser) {
			fmt.Fprintln(out, "OPF cancelled; refs stay queued for the next push.")
			return nil
		}
		return fmt.Errorf("push migrated refs: %w", err)
	}
	switch {
	case pushDisabled:
		// Confirmed, but checkpoint pushing is disabled in settings, so nothing
		// went to the remote. The refs stay queued locally.
		fmt.Fprintln(out, "Checkpoint pushing is disabled in settings; refs stay queued for the next push.")
	case pushed == 0:
		// Enabled, but the queue was already empty — e.g. a concurrent git push
		// flushed the just-migrated refs while we prompted.
		fmt.Fprintln(out, "No queued refs to push (they may have already been pushed).")
	default:
		fmt.Fprintf(out, "Pushed %d checkpoint ref(s).\n", pushed)
	}
	return nil
}

// resolveMigratePushRemote picks the remote migrated refs push to: the
// explicit --remote value if given, else the elected checkpoint sync
// remote. Fail-closed (spec: non-hook drain paths): a misconfigured
// checkpoint_push_remote is an error, never a fallback to origin.
func resolveMigratePushRemote(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	syncRemote, err := strategy.ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot determine checkpoint sync remote (pass --remote explicitly): %w", err)
	}
	if syncRemote.Name == "" {
		return "", errors.New("no git remotes configured; pass --remote explicitly")
	}
	return syncRemote.Name, nil
}
