package cli

import (
	"errors"
	"fmt"
	"io"

	git "github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// oversizedMetadataListCap bounds how many oversized files doctor lists by
// name; the count line carries the rest.
const oversizedMetadataListCap = 5

// oversizedMetadataThreshold is the size doctor treats as oversized. A
// variable so tests can lower it instead of writing 50 MiB fixtures.
var oversizedMetadataThreshold = strategy.OversizedCheckpointMetadataThreshold

// shrinkMetadataCommand is the explicit opt-in for the repair. It is a
// subcommand rather than part of `doctor --force` because this fix rewrites
// history and force-pushes a branch: `--force` is "apply every fix without
// asking", is run routinely by scripts and the e2e harness, and must not
// acquire a remote write as a side effect. An interactive `entire doctor`
// still offers it, because a human answering the prompt is the opt-in.
const shrinkMetadataCommand = "entire doctor shrink-checkpoint-metadata"

// checkOversizedCheckpointMetadata reports metadata.json blobs on
// entire/checkpoints/v1 that GitHub will refuse. Interactively it offers the
// rewrite; otherwise it names the subcommand that applies it. It never acts
// under a bare `--force`.
//
// Only the git-branch backend is inspected — with git-refs as the primary store
// the v1 branch is no longer what gets pushed.
func checkOversizedCheckpointMetadata(cmd *cobra.Command) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft: a bad checkpoints block already surfaces elsewhere; default to inspecting the branch
		fmt.Fprintln(w, "✓ Checkpoint metadata size: skipped (git-refs store is primary)")
		return nil
	}
	repo, err := openRepository(ctx)
	if err != nil {
		return err
	}
	defer repo.Close()

	remoteName, scan, err := scanCheckpointMetadataSize(cmd, repo)
	if err != nil {
		return err
	}
	if scan.Empty() {
		if scan.RemoteErr != nil {
			// The local branch is clean but the remote side is unknown, which
			// is not the same as clean: say so instead of a bare OK.
			fmt.Fprintf(w, "○ Checkpoint metadata size: local %s OK; %s could not be read, so its history was not checked\n", paths.MetadataBranchName, remoteName)
			return nil
		}
		fmt.Fprintln(w, "✓ Checkpoint metadata size: OK")
		return nil
	}
	reportOversizedCheckpointMetadata(w, scan, remoteName)
	if scan.DedicatedCheckpointRemote {
		return nil
	}

	// Degrade to report-only when there is nobody to ask. Unlike the other
	// checks this deliberately does not honor --force; see shrinkMetadataCommand.
	if !interactive.CanPromptInteractively() {
		fmt.Fprintf(w, "  Run `%s` to apply it.\n", shrinkMetadataCommand)
		return nil
	}
	proceed, promptErr := confirmDoctorFix(ctx, w, "Rewrite the checkpoint branch to drop the oversized field?")
	if promptErr != nil {
		return promptErr
	}
	if !proceed {
		return nil
	}
	return applyCheckpointMetadataShrink(cmd, repo, scan, remoteName)
}

// newDoctorShrinkCheckpointMetadataCmd is the non-interactive path to the
// repair that checkOversizedCheckpointMetadata reports.
func newDoctorShrinkCheckpointMetadataCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "shrink-checkpoint-metadata",
		Short: "Rewrite entire/checkpoints/v1 without oversized prompt_attributions fields and force-push it",
		Long: `Repair the checkpoint branch when a per-session metadata.json exceeds 50 MiB.

GitHub refuses blobs over 100 MiB, so one such file anywhere in the history of
entire/checkpoints/v1 makes the branch unpushable — and unmirrorable — there.
CLI versions before v0.10.1 produced them by recording every file of nested git
checkouts (agent worktrees) in the prompt_attributions diagnostic field on every
prompt.

The repair rewrites the branch dropping that field from the oversized files.
Nothing reads the field back: checkpoint IDs, transcripts, attribution summaries
and every other file are unchanged, and history before the first oversized blob
keeps its hashes. The rewritten branch is then force-pushed to the checkpoint
sync remote, guarded with --force-with-lease against the tip observed during the
scan, so a remote that moved in between is refused rather than overwritten.

Run it in every clone that has a local entire/checkpoints/v1: another clone that
still holds the old history would replay it onto the remote on its next push.

Interactively it asks before rewriting; pass --yes to skip the prompt.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // same fail-soft default as the check
				fmt.Fprintln(w, "The git-refs store is the primary checkpoint store; the v1 branch is not pushed, so there is nothing to repair.")
				return nil
			}
			repo, err := openRepository(ctx)
			if err != nil {
				return err
			}
			defer repo.Close()
			remoteName, scan, err := scanCheckpointMetadataSize(cmd, repo)
			if err != nil {
				return err
			}
			if scan.Empty() {
				fmt.Fprintln(w, "No oversized checkpoint metadata found — nothing to repair.")
				return nil
			}
			reportOversizedCheckpointMetadata(w, scan, remoteName)
			if scan.DedicatedCheckpointRemote {
				return NewSilentError(strategy.ErrDedicatedCheckpointRemote)
			}
			if !yes {
				if !interactive.CanPromptInteractively() {
					fmt.Fprintln(w, "  Pass --yes to apply it.")
					return nil
				}
				proceed, promptErr := confirmDoctorFix(ctx, w, "Rewrite the checkpoint branch to drop the oversized field?")
				if promptErr != nil {
					return promptErr
				}
				if !proceed {
					return nil
				}
			}
			return applyCheckpointMetadataShrink(cmd, repo, scan, remoteName)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Rewrite and push without prompting")
	return cmd
}

// scanCheckpointMetadataSize resolves the checkpoint sync remote and runs the
// read-only scan, warning (rather than failing) about a remote that cannot be
// read. An election error is reported as such: a checkpoint_push_remote naming
// a missing remote fails closed on purpose and must not read as "no remote".
func scanCheckpointMetadataSize(cmd *cobra.Command, repo *git.Repository) (string, *strategy.MetadataSizeScan, error) {
	ctx := cmd.Context()
	remoteName := ""
	if elected, electErr := strategy.ResolveCheckpointSyncRemote(ctx); electErr == nil {
		remoteName = elected.Name
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: checkpoint sync remote could not be resolved (%v); the remote side is not inspected.\n", electErr)
	}
	scan, err := strategy.ScanOversizedCheckpointMetadata(ctx, repo, remoteName, oversizedMetadataThreshold)
	if err != nil {
		return "", nil, fmt.Errorf("scan checkpoint metadata sizes: %w", err)
	}
	if scan.RemoteErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not read %s's %s: %v\n", remoteName, paths.MetadataBranchName, scan.RemoteErr)
	}
	return remoteName, scan, nil
}

// gitHubBlobHardLimit is the blob size GitHub refuses outright; the scan's
// threshold (50 MiB) is GitHub's warning line, well short of it.
const gitHubBlobHardLimit int64 = 100 << 20

func reportOversizedCheckpointMetadata(w io.Writer, scan *strategy.MetadataSizeScan, remoteName string) {
	fmt.Fprintln(w, "Checkpoint metadata size: OVERSIZED")
	all := scan.All()
	overLimit := 0
	for _, b := range all {
		if b.Size > gitHubBlobHardLimit {
			overLimit++
		}
	}
	fmt.Fprintf(w, "  %d file(s) on %s exceed %s, the size GitHub warns about.\n", len(all), paths.MetadataBranchName, humanBytes(scan.Threshold))
	if overLimit > 0 {
		fmt.Fprintf(w, "  %d of them exceed GitHub's %s hard limit: the branch cannot be pushed or mirrored\n", overLimit, humanBytes(gitHubBlobHardLimit))
		fmt.Fprintln(w, "  there while any of those is in its history.")
	} else {
		fmt.Fprintf(w, "  None exceeds GitHub's %s hard limit yet, so the branch still pushes; the field that\n", humanBytes(gitHubBlobHardLimit))
		fmt.Fprintln(w, "  made them this large is diagnostic only and can be dropped.")
	}
	writeOversizedList(w, scan)
	fmt.Fprintln(w, "  Cause: CLI versions before v0.10.1 recorded every file of nested git checkouts")
	fmt.Fprintln(w, "  (agent worktrees) in the prompt_attributions diagnostic field on every prompt.")
	if scan.DedicatedCheckpointRemote {
		fmt.Fprintln(w, "  This repository pushes the branch to a dedicated checkpoint remote (checkpoint_remote),")
		fmt.Fprintln(w, "  which the automatic repair does not handle yet: the branch has to be rewritten without")
		fmt.Fprintln(w, "  that field and force-pushed to the checkpoint remote by hand.")
		return
	}
	fmt.Fprintf(w, "  Fix: rewrite %s dropping prompt_attributions from those files (nothing reads it\n", paths.MetadataBranchName)
	fmt.Fprintln(w, "  back; checkpoint IDs, transcripts and attribution summaries are unchanged)")
	switch {
	case remoteName == "":
		fmt.Fprintln(w, "  and leave the branch local (no checkpoint sync remote).")
	case scan.PushDisabled:
		fmt.Fprintf(w, "  and leave %s for you to push (checkpoint pushing is disabled in settings).\n", remoteName)
	default:
		fmt.Fprintf(w, "  and force-push it to %s (lease-guarded against the tip observed just now).\n", remoteName)
	}
}

// applyCheckpointMetadataShrink runs the rewrite and reports the outcome,
// distinguishing a completed local rewrite whose push failed from a rewrite
// that did not happen.
func applyCheckpointMetadataShrink(cmd *cobra.Command, repo *git.Repository, scan *strategy.MetadataSizeScan, remoteName string) error {
	w := cmd.OutOrStdout()
	res, err := strategy.ShrinkOversizedCheckpointMetadata(cmd.Context(), repo, scan, w)
	if err != nil {
		var pushErr *strategy.PushFailedError
		if errors.As(err, &pushErr) {
			fmt.Fprintf(w, "  Local %s was rewritten (%d commit(s), %d file(s) shrunk) but the push to %s failed.\n", paths.MetadataBranchName, res.CommitsRewritten, res.BlobsShrunk, pushErr.Remote)
			fmt.Fprintf(w, "  Re-run `%s` once the remote is reachable; it pushes the rewritten branch.\n", shrinkMetadataCommand)
		}
		return fmt.Errorf("shrink checkpoint metadata: %w", err)
	}
	switch {
	case res.CommitsRewritten > 0 && res.RemoteCommitsRewritten > 0:
		fmt.Fprintf(w, "  ✓ Fixed: rewrote %d local and %d remote commit(s), shrank %d file(s)\n", res.CommitsRewritten, res.RemoteCommitsRewritten, res.BlobsShrunk)
	case res.RemoteCommitsRewritten > 0:
		fmt.Fprintf(w, "  ✓ Fixed: rewrote %d commit(s) of %s's history, shrank %d file(s)\n", res.RemoteCommitsRewritten, remoteName, res.BlobsShrunk)
	default:
		fmt.Fprintf(w, "  ✓ Fixed: rewrote %d commit(s), shrank %d file(s)\n", res.CommitsRewritten, res.BlobsShrunk)
	}
	switch {
	case res.Pushed:
		fmt.Fprintf(w, "  ✓ Pushed %s to %s\n", paths.MetadataBranchName, remoteName)
	case res.PushSkippedReason != "":
		fmt.Fprintf(w, "  Not pushed: %s\n", res.PushSkippedReason)
	}
	for _, b := range res.StillOversized {
		fmt.Fprintf(w, "  Warning: %s is still %s after the rewrite; it is large for another reason.\n", b.Path, humanBytes(b.Size))
	}
	if remoteName != "" {
		fmt.Fprintf(w, "  Run `%s` in every other clone that has a local %s, or its next push replays the old history onto %s.\n", shrinkMetadataCommand, paths.MetadataBranchName, remoteName)
	}
	return nil
}

func writeOversizedList(w io.Writer, scan *strategy.MetadataSizeScan) {
	remoteOnly := make(map[string]struct{}, len(scan.Remote))
	for _, b := range scan.Remote {
		remoteOnly[b.Hash.String()] = struct{}{}
	}
	all := scan.All()
	for i, b := range all {
		if i == oversizedMetadataListCap {
			fmt.Fprintf(w, "    ... and %d more\n", len(all)-oversizedMetadataListCap)
			break
		}
		where := ""
		if _, ok := remoteOnly[b.Hash.String()]; ok {
			where = fmt.Sprintf(" (only on %s)", scan.RemoteName)
		}
		fmt.Fprintf(w, "    %8s  %s%s\n", humanBytes(b.Size), b.Path, where)
	}
}

// humanBytes renders a byte count in binary units, the ones GitHub's limits
// are stated in.
func humanBytes(n int64) string {
	const unit = 1024
	switch {
	case n >= unit*unit*unit:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(unit*unit*unit))
	case n >= unit*unit:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(unit*unit))
	case n >= unit:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(unit))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
