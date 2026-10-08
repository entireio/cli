package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"
)

func newSessionShareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share [session-id]",
		Short: "Publish a session's work so far and print the command to resume it",
		Long: `Create a checkpoint from a session, push it, and print the resume command.

Sharing needs neither a commit nor a branch. A session that only read code and
reasoned about it — an investigation, a review, a dead end worth handing over —
produces a checkpoint like any other, and the resume command works from it
alone. The recipient does not need your branch, and does not need the
checkpoint fetched in advance: naming it is enough.

This is 'entire checkpoint create' plus the push. The checkpoint is a snapshot:
your session's own checkpoint window is left alone, so the session stays
exactly as live, as committable, and as resumable as it was before, and your
next commit's checkpoint still covers the same work.

A session with uncommitted file changes is refused. Your next commit
checkpoints that work with full attribution, and sharing now would publish a
weaker copy of it. Commit first, then share — or share from a session that
changed nothing, which is the case this command is for.

With no argument, shares the session running this command. Entire only shares a
session it can positively identify as the caller, because sharing publishes a
transcript; when it cannot, it says so and asks for the session ID rather than
guessing at the most recent one.

Examples:
  entire session share
  entire session share 2026-09-30-8f76b0e8-b8f1-4a87-9186-848bdd83d62e`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if checkDisabledGuard(ctx, cmd.OutOrStdout()) {
				return nil
			}
			cmd.SilenceUsage = true
			return runSessionShare(ctx, cmd, args)
		},
	}

	return cmd
}

func runSessionShare(ctx context.Context, cmd *cobra.Command, args []string) error {
	w := cmd.OutOrStdout()

	// Shares the session resolution `checkpoint create` uses: sharing acts on a
	// session, so only a resolution that identified the caller is accepted. The
	// worktree and other-worktree tiers are "most recent session" guesses, and
	// acting on a wrong guess here publishes somebody else's transcript.
	sessionID, _, err := resolveCheckpointCreateSession(cmd, args)
	if err != nil {
		return err
	}

	// Resolved before the checkpoint is written: a repo with no remote cannot
	// share at all, and failing after the write would leave a checkpoint behind
	// with nothing to show for it.
	remote, err := resolveShareRemote(ctx)
	if err != nil {
		return err
	}

	// Required by both halves. Checkpoint writes must not fall back to the
	// default scanner set on a broken redaction config, and the OPF gate inside
	// the push reads the same process-global config — unconfigured, it reads as
	// "OPF off" and would send un-OPF'd content to the remote.
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		return fmt.Errorf("configure redaction: %w", err)
	}
	// The session may belong to an external agent, which condensation can only
	// read once registered.
	external.DiscoverAndRegister(ctx)

	strat := GetStrategy(ctx)
	checkpointID, err := strat.CreateSnapshotCheckpoint(ctx, sessionID)
	if errors.Is(err, strategy.ErrNothingToCheckpoint) {
		fmt.Fprintf(w, "Nothing to share yet: session %s has no transcript or files to checkpoint.\n", sessionID)
		return nil
	}
	if err != nil {
		return describeShareCheckpointError(err)
	}

	fmt.Fprintf(w, "Checkpoint %s written.\n", checkpointID)

	pushed, err := pushSharedCheckpoint(ctx, cmd, strat, remote)
	if err != nil {
		// The checkpoint is written and stays queued, so this is a delivery
		// failure and not a lost one. Say which, or the user re-runs share and
		// wonders why nothing changed.
		fmt.Fprintln(cmd.ErrOrStderr(),
			"\nThe checkpoint is written locally and stays queued for the next push.\nIt has not reached the remote, so nobody else can resume it yet.")
		return err
	}

	printShareResult(w, cmd, checkpointID, pushed)
	return nil
}

// describeShareCheckpointError turns the snapshot writer's refusal into
// guidance the user can act on, and passes anything else through unchanged.
//
// Only the refusal is translated. ErrPendingFileChanges is a decision rather
// than a fault — the next commit checkpoints that work with attribution — and
// the sentinel's own wording states the condition without saying what to do
// about it.
func describeShareCheckpointError(err error) error {
	if errors.Is(err, strategy.ErrPendingFileChanges) {
		return errors.New(
			"this session has uncommitted file changes, and your next commit will checkpoint them with attribution\nCommit first, then share")
	}
	return err
}

func pushSharedCheckpoint(ctx context.Context, cmd *cobra.Command, strat *strategy.ManualCommitStrategy, remote string) (strategy.ShareCheckpointResult, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return strategy.ShareCheckpointResult{}, fmt.Errorf("not a git repository: %w", err)
	}
	defer repo.Close()

	result, err := strat.PushSharedCheckpoints(ctx, repo, remote)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return result, NewSilentError(err)
		}
		// Declining OPF is a decision, not a failure: nothing shipped, and the
		// checkpoint stays queued for the next push.
		if errors.Is(err, strategy.ErrOPFAbortedByUser) {
			// Printed here, so the error must not print again: the convention
			// is NewSilentError after custom output (see errors.go).
			fmt.Fprintln(cmd.ErrOrStderr(), "OPF cancelled; the checkpoint stays queued for the next push.")
			return result, NewSilentError(err)
		}
		return result, fmt.Errorf("push checkpoint: %w", err)
	}
	return result, nil
}

// resolveShareRemote returns the elected checkpoint sync remote, which is
// where a shared checkpoint has to go and the only place it can usefully go.
//
// There is deliberately no override. Reads resolve through the same election
// (CheckpointReadRemotes: the elected remote, then origin), so a flag that
// moved only the push would strand the checkpoint somewhere the printed resume
// command never looks. Directing checkpoint traffic elsewhere is the
// checkpoint_push_remote setting's job, which moves reads with it.
//
// Fail-closed, matching the other non-hook push paths: a misconfigured
// checkpoint_push_remote is an error, never a silent fallback to origin.
func resolveShareRemote(ctx context.Context) (string, error) {
	syncRemote, err := strategy.ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot determine the checkpoint remote to share to: %w", err)
	}
	if syncRemote.Name == "" {
		return "", errors.New("no git remotes configured, so there is nowhere to share to")
	}
	return syncRemote.Name, nil
}

// shareRootName is the binary name used in the resume line printed for the
// recipient. Taken from the running command tree so a renamed or vendored
// binary prints its own name, and falling back to the canonical one when the
// command is not attached to a root (unit tests build it standalone).
func shareRootName(cmd *cobra.Command) string {
	if cmd == nil || cmd.Root() == cmd {
		return cmdRoot
	}
	return cmd.Root().Name()
}

func printShareResult(w io.Writer, cmd *cobra.Command, checkpointID id.CheckpointID, pushed strategy.ShareCheckpointResult) {
	resumeCmd := fmt.Sprintf("%s %s resume %s", shareRootName(cmd), cmdSession, checkpointID)

	if pushed.PushDisabled {
		fmt.Fprintln(w, "\nCheckpoint pushing is disabled in settings, so nothing left this machine.")
		fmt.Fprintf(w, "Anyone with the repository can resume it once it is pushed:\n\n  %s\n", resumeCmd)
		return
	}

	// Both backends report a confirmed count and error out when nothing
	// landed, so reaching here means the checkpoint is on the remote.
	fmt.Fprintf(w, "Pushed %d checkpoint ref(s).\n", pushed.Pushed)
	fmt.Fprintf(w, "\nShare this:\n\n  %s\n", resumeCmd)
}
