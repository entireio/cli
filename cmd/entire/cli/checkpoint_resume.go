package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

var errNoResumeCommit = errors.New("no commit found")

func newCheckpointResumeCmd() *cobra.Command {
	var checkpointFlag string
	var commitFlag string
	var branchFlag string
	var force bool

	cmd := &cobra.Command{
		Use:    "resume [checkpoint-id | commit-sha | branch]",
		Short:  "Resume the agent session(s) recorded in a checkpoint",
		Hidden: true,
		Long: `Resume agent sessions from a committed checkpoint.

The target can be a checkpoint ID (or prefix), a commit SHA (or ref) whose
message carries an Entire-Checkpoint trailer, or a branch name. Auto-detection
tries checkpoint ID first, then local branch, then commit, then remote branch
(offering to fetch it); use the flags to force one interpretation.

For a checkpoint or commit target, the branch containing the checkpoint's
commit is checked out at its current tip before the session logs are
restored. If no local branch contains it, the session logs are restored
without switching branches. A branch target checks the branch out and
resumes its latest checkpoint.

With no target, shows recent checkpoints: an interactive picker on a
terminal, a plain-text list otherwise.

Existing local session logs are never overwritten unless --force is given.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			var positional string
			if len(args) > 0 {
				positional = args[0]
				if checkpointFlag != "" || commitFlag != "" || branchFlag != "" {
					return errors.New("cannot combine positional argument with --checkpoint, --commit, or --branch")
				}
			}
			if _, err := paths.WorktreeRoot(cmd.Context()); err == nil {
				logging.SetLogLevelGetter(GetLogLevel)
				if err := logging.Init(cmd.Context(), ""); err == nil {
					defer logging.Close()
				}
			}
			external.DiscoverAndRegister(cmd.Context())
			return runCheckpointResume(cmd.Context(), cmd, positional, checkpointFlag, commitFlag, branchFlag, force)
		},
	}

	cmd.Flags().StringVarP(&checkpointFlag, "checkpoint", "c", "", "Resume a specific checkpoint (ID or prefix)")
	cmd.Flags().StringVar(&commitFlag, "commit", "", "Resume the checkpoint referenced by a commit (SHA or ref)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Resume the latest checkpoint on a branch")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmations and overwrite existing local session logs")
	cmd.MarkFlagsMutuallyExclusive("checkpoint", "commit", "branch")

	return cmd
}

func runCheckpointResume(ctx context.Context, cmd *cobra.Command, target, checkpointFlag, commitFlag, branchFlag string, force bool) error {
	if branchFlag != "" {
		return runResume(ctx, cmd, branchFlag, force)
	}

	lookup, err := newExplainCheckpointLookup(ctx)
	if err != nil {
		return err
	}
	initialLookup := lookup
	defer func() {
		if lookup != nil && lookup != initialLookup {
			_ = lookup.Close()
		}
		_ = initialLookup.Close()
	}()

	switch {
	case checkpointFlag != "":
		var matches []id.CheckpointID
		matches, lookup = matchCheckpointPrefixWithRemoteFallback(ctx, cmd.ErrOrStderr(), lookup, checkpointFlag)
		return resumeMatchedCheckpoints(ctx, cmd, lookup, checkpointFlag, matches, force)
	case commitFlag != "":
		return resumeCommitTarget(ctx, cmd, lookup, commitFlag, force)
	case target != "":
		return resumeAutoTarget(ctx, cmd, lookup, target, force)
	default:
		return runCheckpointResumePicker(ctx, cmd, lookup, force)
	}
}

// resumeAutoTarget resolves a positional target, trying in order: local
// checkpoint-ID prefix, local branch, remote checkpoint fallback, commit
// revision, and finally remote branch — resuming another machine's work
// usually means the branch isn't local yet, so runResume offers to fetch it.
// The local branch check runs before the remote checkpoint fetch so branch
// names never pay a network round-trip, and remote branches are tried only
// after commit resolution so revision syntax like HEAD (which would match
// refs/remotes/origin/HEAD) cannot be misrouted into the branch flow. A
// lookup swapped in by the remote fallback is closed here; the caller keeps
// ownership of the lookup it passed in.
func resumeAutoTarget(ctx context.Context, cmd *cobra.Command, lookup *explainCheckpointLookup, target string, force bool) error {
	// Targets that can't be checkpoint IDs (e.g. "feature/foo") skip the
	// store lookup and its remote-fetch fallback entirely.
	shapedLikeCheckpoint := id.CouldBePrefix(target)
	if shapedLikeCheckpoint {
		if matches := matchCheckpointPrefix(lookup, target); len(matches) > 0 {
			return resumeMatchedCheckpoints(ctx, cmd, lookup, target, matches, force)
		}
	}

	if branchExistsLocally(lookup.repo, target) {
		return runResume(ctx, cmd, target, force)
	}

	if shapedLikeCheckpoint {
		matches, fresh := matchCheckpointPrefixWithRemoteFallback(ctx, cmd.ErrOrStderr(), lookup, target)
		if fresh != lookup {
			defer func() { _ = fresh.Close() }()
			lookup = fresh
		}
		if len(matches) > 0 {
			return resumeMatchedCheckpoints(ctx, cmd, lookup, target, matches, force)
		}
	}

	err := resumeCommitTarget(ctx, cmd, lookup, target, force)
	if errors.Is(err, errNoResumeCommit) {
		if remoteExists, remoteErr := BranchExistsOnRemote(ctx, target); remoteErr == nil && remoteExists {
			return runResume(ctx, cmd, target, force)
		}
		return fmt.Errorf("nothing matched %q as a checkpoint ID, branch, or commit\nHint: run 'entire checkpoint list' to see available checkpoints", target)
	}
	return err
}

func resumeMatchedCheckpoints(ctx context.Context, cmd *cobra.Command, lookup *explainCheckpointLookup, prefix string, matches []id.CheckpointID, force bool) error {
	switch len(matches) {
	case 0:
		return fmt.Errorf("no committed checkpoint matched %q\nHint: run 'entire checkpoint list' to see available checkpoints", prefix)
	case 1:
		return resumeResolvedCheckpoint(ctx, cmd, lookup, matches[0], force)
	default:
		renderAmbiguousPrefixFailure(cmd.ErrOrStderr(), prefix, "committed checkpoints", buildAmbiguousCheckpointMatches(matches, lookup.committed))
		return NewSilentError(fmt.Errorf("%w: checkpoint prefix %s matches %d checkpoints", errAmbiguousCommitPrefix, prefix, len(matches)))
	}
}

// resumeCommitTarget resolves ref to a commit and resumes the checkpoint its
// Entire-Checkpoint trailer references. Multiple trailers (squash merge)
// resolve to the newest checkpoint by CreatedAt.
func resumeCommitTarget(ctx context.Context, cmd *cobra.Command, lookup *explainCheckpointLookup, ref string, force bool) error {
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	hash, ambiguousMatches, err := resolveCommitUnambiguous(lookup.repo, ref)
	if err != nil {
		if errors.Is(err, errAmbiguousCommitPrefix) {
			renderAmbiguousPrefixFailure(errW, ref, "commits", buildAmbiguousCommitMatches(lookup.repo, ambiguousMatches))
			return NewSilentError(err)
		}
		logging.Debug(ctx, "checkpoint resume: commit resolution failed",
			slog.String("ref", ref),
			slog.String("error", err.Error()))
		return fmt.Errorf("%w matching %q", errNoResumeCommit, ref)
	}
	commit, err := lookup.repo.CommitObject(hash)
	if err != nil {
		return fmt.Errorf("failed to get commit %s: %w", abbreviateCommitHash(lookup.repo, hash), err)
	}

	cpIDs := trailers.ParseAllCheckpoints(commit.Message)
	if len(cpIDs) == 0 {
		printNoTrailerMessage(w, lookup.repo, hash)
		return NewSilentError(fmt.Errorf("commit %s has no Entire-Checkpoint trailer", abbreviateCommitHash(lookup.repo, hash)))
	}

	cpID := cpIDs[0]
	if len(cpIDs) > 1 {
		latest, found, latestErr := resolveLatestCheckpoint(ctx, lookup.store, cpIDs)
		if latestErr != nil {
			return latestErr
		}
		if found {
			cpID = latest.CheckpointID
		}
	}
	return resumeResolvedCheckpoint(ctx, cmd, lookup, cpID, force)
}

// resumeResolvedCheckpoint resumes one committed checkpoint: checks out the
// branch containing it (at the branch's current tip) when one exists, points
// at the owning worktree when that branch is checked out elsewhere, and falls
// back to restoring session logs in place when no local branch contains it.
func resumeResolvedCheckpoint(ctx context.Context, cmd *cobra.Command, lookup *explainCheckpointLookup, cpID id.CheckpointID, force bool) error {
	w := cmd.OutOrStdout()

	branch := buildCheckpointBranchIndex(lookup.repo)[cpID.String()]
	if branch == "" {
		fmt.Fprintf(w, "Checkpoint %s is not on any local branch; restoring session logs without switching branches.\n", cpID)
		return resumeByCheckpointID(ctx, w, cmd.ErrOrStderr(), cpID, force)
	}
	if otherPath, ok := branchCheckedOutElsewhere(ctx, branch); ok {
		fmt.Fprintf(w, "Branch %q is already checked out at %s.\nResume this checkpoint from that worktree:\n\n  cd %s && entire checkpoint resume %s\n",
			branch, otherPath, shellQuote(otherPath), cpID)
		return nil
	}
	return resumeSessionOnBranch(ctx, cmd, branch, cpID, force)
}

const checkpointResumePickerLimit = 20

func runCheckpointResumePicker(ctx context.Context, cmd *cobra.Command, lookup *explainCheckpointLookup, force bool) error {
	w := cmd.OutOrStdout()

	// store.List (behind lookup.committed) already returns checkpoints
	// newest-first; see checkpoint.sortCheckpointInfosByRecency.
	entries := lookup.committed
	if len(entries) > checkpointResumePickerLimit {
		entries = entries[:checkpointResumePickerLimit]
	}
	if len(entries) == 0 {
		fmt.Fprintln(w, "No committed checkpoints found.")
		fmt.Fprintln(w, "Checkpoints are created when you commit during an agent session.")
		return nil
	}
	branchIndex := buildCheckpointBranchIndex(lookup.repo)

	if !interactive.CanPromptInteractively() {
		printCheckpointResumeList(w, entries, branchIndex)
		return nil
	}

	selected, ok, err := promptCheckpointSelection(ctx, entries, branchIndex)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(w, "Resume cancelled.")
		return nil
	}
	return resumeResolvedCheckpoint(ctx, cmd, lookup, selected, force)
}

func printCheckpointResumeList(w io.Writer, entries []checkpoint.CheckpointInfo, branchIndex map[string]string) {
	fmt.Fprintf(w, "Recent checkpoints (newest first, up to %d):\n\n", checkpointResumePickerLimit)
	for _, e := range entries {
		branch := branchIndex[e.CheckpointID.String()]
		if branch == "" {
			branch = "-"
		}
		fmt.Fprintf(w, "  %s  %s  %s  %s\n", e.CheckpointID, e.CreatedAt.Local().Format("2006-01-02 15:04"), branch, checkpointAgentLabel(e))
	}
	fmt.Fprintln(w, "\nResume one with: entire checkpoint resume <checkpoint-id>")
}

func promptCheckpointSelection(ctx context.Context, entries []checkpoint.CheckpointInfo, branchIndex map[string]string) (id.CheckpointID, bool, error) {
	options := make([]huh.Option[string], 0, len(entries)+1)
	for _, e := range entries {
		options = append(options, huh.NewOption(checkpointResumeOptionLabel(e, branchIndex), e.CheckpointID.String()))
	}
	options = append(options, huh.NewOption("Cancel", resumePickerCancel))

	var choice string
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Resume which checkpoint?").
			Description("Checks out the checkpoint's branch (when one contains it) and restores the session log.").
			Options(options...).
			Value(&choice),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to pick checkpoint: %w", err)
	}
	if choice == resumePickerCancel {
		return "", false, nil
	}
	return id.CheckpointID(choice), true, nil
}

func checkpointResumeOptionLabel(e checkpoint.CheckpointInfo, branchIndex map[string]string) string {
	branch := branchIndex[e.CheckpointID.String()]
	if branch == "" {
		branch = "no local branch"
	}
	return fmt.Sprintf("%s · %s · %s · %s", e.CheckpointID.DisplayShort(), branch, checkpointAgentLabel(e), timeAgo(e.CreatedAt))
}

func checkpointAgentLabel(e checkpoint.CheckpointInfo) string {
	if e.Agent == "" {
		return unknownAgentLabel
	}
	return string(e.Agent)
}
