package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"charm.land/huh/v2"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

Each checkpoint is taken from whichever copy of the v1 branch, this clone's or
a remote's, changed it last. When both changed it since they split, neither
copy can become the ref without hiding the other's sessions, so in a terminal
the command asks which copy to keep; without one it writes no ref for that
checkpoint, lists it, and exits 1.

After the switch to the git-refs store the refs are authoritative, so the
command only resolves checkpoints that still have no ref because their copies
differ. Saving a session or attaching one to such a checkpoint fails until it
is resolved. A ref a remote already holds is fetched instead of asking.

New refs are queued for push. Run interactively, it asks whether to push them
now; non-interactively it never pushes — the refs stay queued and flush on the
next push once the git-refs store is the configured primary.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrateCheckpoints(cmd.Context(), cmd.OutOrStdout(), dryRun, remote)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be migrated without writing refs")
	cmd.Flags().StringVar(&remote, "remote", "", "Remote to push migrated refs to (default: the checkpoint sync remote)")
	return cmd
}

func runMigrateCheckpoints(ctx context.Context, out io.Writer, dryRun bool, remote string) error {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()

	canAsk := interactive.CanPromptInteractively()
	opts := checkpoint.MigrateOptions{DryRun: dryRun}
	if canAsk {
		opts.Choose = func(ctx context.Context, d checkpoint.DivergedLegacyCheckpoint) (plumbing.ReferenceName, error) {
			return chooseLegacyCopy(ctx, out, d)
		}
	}

	// Once git-refs is the primary store the refs are authoritative and the v1
	// branch may lag behind them; re-importing its snapshots could only regress
	// refs. What is left is the checkpoints the write path refuses to copy.
	afterSwitch := false
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft: a bad checkpoints block already surfaces via Open; default to allowing migration
		afterSwitch = true
	}
	var result checkpoint.MigrateResult
	if afterSwitch {
		result, err = checkpoint.ResolveDivergedLegacyCheckpoints(ctx, repo, opts, FetchCheckpointRef)
	} else {
		result, err = checkpoint.MigrateBranchToRefs(ctx, repo, opts)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, huh.ErrUserAborted) {
			return NewSilentError(err)
		}
		return fmt.Errorf("migrate checkpoints: %w", err)
	}

	reportMigrateResult(out, result, dryRun, afterSwitch)
	unresolved := reportDivergedCheckpoints(out, result.Diverged, canAsk)
	if !dryRun && len(result.Migrated) > 0 {
		if err := offerMigratedPush(ctx, out, repo, remote, len(result.Migrated), canAsk); err != nil {
			return err
		}
	}
	if unresolved != nil && !dryRun {
		return NewSilentError(unresolved)
	}
	return nil
}

func reportMigrateResult(out io.Writer, result checkpoint.MigrateResult, dryRun, afterSwitch bool) {
	switch {
	case afterSwitch && len(result.Migrated) == 0 && len(result.Diverged) == 0:
		fmt.Fprintln(out, "The git-refs store is already the primary checkpoint store — nothing to migrate.")
	case afterSwitch:
		if len(result.Migrated) > 0 {
			fmt.Fprintf(out, "Wrote the chosen copy of %d checkpoint(s) to refs.\n", len(result.Migrated))
		}
	case result.Total == 0:
		fmt.Fprintln(out, "No checkpoints found on the v1 branch — nothing to migrate.")
	default:
		verb := "Migrated"
		if dryRun {
			verb = "Would migrate"
		}
		fmt.Fprintf(out, "%s %d checkpoint(s) to refs (%d already up to date, %d total).\n",
			verb, len(result.Migrated), result.Skipped, result.Total)
	}
}

// reportDivergedCheckpoints lists the checkpoints left without a ref because
// their v1 copies differ and returns an error naming them, or nil when there
// are none.
func reportDivergedCheckpoints(out io.Writer, diverged []checkpoint.DivergedLegacyCheckpoint, canAsk bool) error {
	if len(diverged) == 0 {
		return nil
	}
	if len(diverged) == 1 {
		fmt.Fprintf(out, "1 checkpoint differs between %s and needs you to choose a copy:\n", divergedCopiesLabel(diverged))
	} else {
		fmt.Fprintf(out, "%d checkpoints differ between %s and need you to choose a copy:\n", len(diverged), divergedCopiesLabel(diverged))
	}
	for _, d := range diverged {
		fmt.Fprintf(out, "  %s\n", d.ID)
	}
	if canAsk {
		fmt.Fprintln(out, "Run 'entire doctor migrate-checkpoints' again to choose.")
	} else {
		fmt.Fprintln(out, "Run 'entire doctor migrate-checkpoints' in a terminal to choose.")
	}
	return fmt.Errorf("%d checkpoint(s) need a copy chosen", len(diverged))
}

// divergedCopiesLabel names the two sides the listed checkpoints differ
// between, naming the remote when every checkpoint differs from the same one.
func divergedCopiesLabel(diverged []checkpoint.DivergedLegacyCheckpoint) string {
	remote := diverged[0].Remote.RemoteName()
	for _, d := range diverged[1:] {
		if d.Remote.RemoteName() != remote {
			remote = ""
			break
		}
	}
	if remote == "" {
		return "this clone and a remote"
	}
	return "this clone and " + remote
}

// chooseLegacyCopy shows both copies of a diverged checkpoint and asks which
// one becomes its ref. It returns "" to skip the checkpoint.
func chooseLegacyCopy(ctx context.Context, out io.Writer, d checkpoint.DivergedLegacyCheckpoint) (plumbing.ReferenceName, error) {
	local, remote := legacyCopyLabel(d.Local), legacyCopyLabel(d.Remote)
	width := max(len(local), len(remote)) + 1
	differing := d.DifferingSessions()
	fmt.Fprintf(out, "Checkpoint %s differs between %s and %s.\n", d.ID, strings.ToLower(local[:1])+local[1:], remote)
	fmt.Fprintf(out, "  %-*s %s\n", width, local+":", legacySessionList(d.Local.SessionIDs, differing))
	fmt.Fprintf(out, "  %-*s %s\n", width, remote+":", legacySessionList(d.Remote.SessionIDs, differing))
	if len(differing) > 0 {
		fmt.Fprintln(out, "  (changed) sessions are in both copies with different content.")
	}
	fmt.Fprintf(out, "  The copy you don't keep stays only in %s history.\n", paths.MetadataBranchName)

	const skip = ""
	var choice string
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Keep which copy?").
			Options(
				huh.NewOption(local, d.Local.Ref.String()),
				huh.NewOption(remote, d.Remote.Ref.String()),
				huh.NewOption("Skip", skip),
			).
			Value(&choice),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("choose a copy of checkpoint %s: %w", d.ID, err)
	}
	return plumbing.ReferenceName(choice), nil
}

// legacySessionList joins sessionIDs, marking those in differing. The IDs come
// from a remote's copy of the checkpoint, so control and format characters
// are dropped: an escape sequence must not redraw the prompt below them.
func legacySessionList(sessionIDs, differing []string) string {
	if len(sessionIDs) == 0 {
		return "(sessions not available locally)"
	}
	listed := make([]string, len(sessionIDs))
	for i, sessionID := range sessionIDs {
		listed[i] = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, sessionID)
		if slices.Contains(differing, sessionID) {
			listed[i] += " (changed)"
		}
	}
	return strings.Join(listed, ", ")
}

func legacyCopyLabel(c checkpoint.LegacyCheckpointCopy) string {
	if name := c.RemoteName(); name != "" {
		return name
	}
	return "This clone"
}

// offerMigratedPush asks whether to push the refs just written, or says they
// stay queued when there is nobody to ask.
func offerMigratedPush(ctx context.Context, out io.Writer, repo *git.Repository, remote string, written int, canAsk bool) error {
	if !canAsk {
		fmt.Fprintln(out, "Refs are queued; they push on the next `git push` once git-refs is the primary store.")
		return nil
	}
	pushRemote, err := resolveMigratePushRemote(ctx, remote)
	if err != nil {
		return err
	}
	confirmed, err := confirmDoctorFix(ctx, out, fmt.Sprintf("Push %d migrated checkpoint ref(s) now?", written))
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(out, "Refs stay queued for the next push.")
		return nil
	}
	return pushMigratedRefs(ctx, out, repo, pushRemote)
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
		if errors.Is(err, strategy.ErrCheckpointPushHeldByPolicy) {
			fmt.Fprintln(out, "Checkpoint push policy is manual; refs stay queued. Upload them with 'entire checkpoint push --all'.")
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

// withLegacyDivergedFix turns a write refused because a legacy checkpoint's v1
// copies both changed it into the step that resolves it. Other errors pass
// through unchanged.
func withLegacyDivergedFix(err error) error {
	var diverged *checkpoint.LegacyCheckpointDivergedError
	if !errors.As(err, &diverged) {
		return err
	}
	remote := diverged.RemoteName()
	if remote == "" {
		remote = "a remote"
	}
	return &legacyDivergedFixError{
		msg: fmt.Sprintf("checkpoint %s has different copies on this clone and %s. "+
			"Run 'entire doctor migrate-checkpoints' to choose one.", diverged.CheckpointID, remote),
		err: err,
	}
}

// legacyDivergedFixError replaces the refused write's message with the fix
// while keeping it inspectable with errors.Is and errors.As.
type legacyDivergedFixError struct {
	msg string
	err error
}

func (e *legacyDivergedFixError) Error() string { return e.msg }
func (e *legacyDivergedFixError) Unwrap() error { return e.err }
