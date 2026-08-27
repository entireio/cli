package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var forceFlag bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose and fix session issues",
		Long: `Scan for session issues and offer to fix them.

Checks performed:
  1. Disconnected metadata branches: detects when local and remote
     entire/checkpoints/v1 branches share no common ancestor (caused by a
     previous bug). Fixes by cherry-picking local checkpoints onto remote tip.

  2. Operational logs: warn when .entire/logs cannot be written. Every other
     diagnostic is delivered by writing there, and that write is silent about
     its own failure, so an unwritable log directory looks exactly like a repo
     where nothing ran.

  When Codex hooks are installed:
  3. Codex hook trust: warn when hooks declared in .codex/hooks.json
     lack a trusted_hash entry in the user's Codex config (i.e. /hooks
     review hasn't run yet on this machine, or a newer entire release
     added a hook the user hasn't approved yet).

  For each installed agent that reports hook-config drift:
  4. Hook config: warn when the installed hooks no longer match what this
     CLI writes (e.g. an older release wrote Claude Code tool matchers that
     no longer fire, or a committed Pi/OpenCode extension has gone stale).
     Fix by re-running 'entire enable --force'.

  5. Stuck sessions: sessions stuck in ACTIVE or ENDED phase that need cleanup.

A session is considered stuck if:
  - It is in ACTIVE phase with no interaction for over 1 hour
  - It is in ENDED phase with uncondensed checkpoint data on a shadow branch

For each stuck session, you can choose to:
  - Condense: Save session data to permanent storage
  - Discard: Remove the session state and shadow branch data
  - Skip: Leave the session as-is

Use --force to condense all fixable sessions without prompting.  Sessions that can't
be condensed will be discarded.

Without a terminal to prompt on (agents, CI), doctor reports each issue and
points at --force instead of prompting.`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			// Cobra runs the persistent pre-runs before a command's own PreRunE,
			// so the root hook has already put an initialized logger in this
			// context: redaction diagnostics and the load-time summary land in
			// .entire/logs/, which is where `entire doctor bundle` collects them
			// and where a user debugging custom rules greps for
			// component=redaction. Answering "did my rules load?" is doctor's
			// job, so it must not be the one command whose diagnostics go to
			// bare stderr.
			return strategy.EnsureRedactionConfigured(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionsFix(cmd, forceFlag)
		},
	}

	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Auto-fix all issues without prompting")

	// Diagnostic subcommands.
	cmd.AddCommand(newTraceCmd())
	cmd.AddCommand(newDoctorLogsCmd())
	cmd.AddCommand(newDoctorBundleCmd())
	cmd.AddCommand(newDoctorMigrateCheckpointsCmd())

	return cmd
}

// stuckSession holds a session state along with diagnostic info.
type stuckSession struct {
	State             *strategy.SessionState
	Reason            string
	ShadowBranch      string
	HasShadowBranch   bool
	CheckpointCount   int
	FilesTouchedCount int
}

func runSessionsFix(cmd *cobra.Command, force bool) error {
	var finalErr error

	// Check 1: Disconnected metadata branches
	metadataErr := checkDisconnectedMetadata(cmd, force)
	if metadataErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Error: metadata check failed: %v\n", metadataErr)
		finalErr = NewSilentError(fmt.Errorf("metadata check failed: %w", metadataErr))
	}

	fmt.Fprintln(cmd.OutOrStdout())

	ctx := cmd.Context()

	// The git hook surface. Checked before the agent hook checks because it is
	// the more fundamental one: if git hooks are broken, commits are not captured
	// at all and agent-config drift is noise by comparison.
	if hooksErr := checkGitHooks(cmd, force); hooksErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Error: git hook check failed: %v\n", hooksErr)
		finalErr = NewSilentError(fmt.Errorf("git hook check failed: %w", hooksErr))
	}

	// Before the remaining checks, because it is the channel they and every
	// other command write their diagnostics to: if this is broken, an empty
	// entire.log is not evidence of a healthy repo.
	checkLogSink(cmd)

	// Agent-specific: Codex hook trust state.
	checkCodexHookTrust(cmd)

	// Agent-specific: Claude Code hook config drift.
	checkHookDrift(cmd)

	// Where checkpoints land, when the repo's remotes make that ambiguous.
	printCheckpointDestinationNote(ctx, cmd.OutOrStdout(), "Checkpoint destination: REVIEW")

	// Stuck sessions
	// Load all session states
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list session states: %w", err)
	}

	if len(states) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No stuck sessions found.")
		if finalErr != nil {
			return finalErr
		}
		return nil
	}

	// Open repository to check shadow branches (uses worktree-aware helper)
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	// Finalize any non-ended session whose agent process has exited (no SessionStop
	// hook fired). A gone process is unambiguous, so these are condensed on the
	// spot rather than left for the interactive prompt below; the sweep marks
	// them ended in place so classifySession won't re-flag them.
	if n := finalizeExitedSessions(ctx, states, time.Now().Add(interactiveSweepCondenseBudget)); n > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Finalized %d exited session(s) (agent process gone).\n\n", n)
	}

	// Identify stuck sessions
	now := time.Now()
	var stuck []stuckSession

	for _, state := range states {
		ss := classifySession(state, repo, now)
		if ss != nil {
			stuck = append(stuck, *ss)
		}
	}

	if len(stuck) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No stuck sessions found.")
		if finalErr != nil {
			return finalErr
		}
		return nil
	}

	// Get the current strategy for condense operations
	strat := GetStrategy(ctx)

	fmt.Fprintf(cmd.OutOrStdout(), "Found %d stuck session(s):\n\n", len(stuck))

	canPrompt := interactive.CanPromptInteractively()

	for _, ss := range stuck {
		displayStuckSession(cmd, ss)

		if force {
			if canCondenseStuckSession(ss) {
				if err := strat.CondenseSessionByID(ctx, ss.State.SessionID); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to condense session %s: %v\n", ss.State.SessionID, err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  ✓ Condensed session %s\n\n", ss.State.SessionID)
				}
			} else {
				// Discard if we can't condense
				if err := discardSession(ctx, ss, repo, cmd.ErrOrStderr()); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to discard session %s: %v\n", ss.State.SessionID, err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  ✓ Discarded session %s\n\n", ss.State.SessionID)
				}
			}
			continue
		}

		// Degrade to warn-only when there is nobody to ask, same as
		// checkGitHooks: an agent or CI run must not crash on a TTY prompt.
		// Disclose what --force would do to this session, using the same
		// predicate the force branch above applies.
		if !canPrompt {
			if canCondenseStuckSession(ss) {
				fmt.Fprintln(cmd.OutOrStdout(), "  Fix: condense to permanent storage.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "  Fix: discard (no condensable checkpoint data).")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "  Run `entire doctor --force` to apply it.")
			fmt.Fprintln(cmd.OutOrStdout())
			continue
		}

		// Interactive: prompt for action
		action, err := promptSessionAction(ss)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return fmt.Errorf("failed to get action: %w", err)
		}

		switch action {
		case "condense":
			if err := strat.CondenseSessionByID(ctx, ss.State.SessionID); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to condense session %s: %v\n", ss.State.SessionID, err)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  ✓ Condensed session %s\n\n", ss.State.SessionID)
			}
		case "discard":
			if err := discardSession(ctx, ss, repo, cmd.ErrOrStderr()); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to discard session %s: %v\n", ss.State.SessionID, err)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  ✓ Discarded session %s\n\n", ss.State.SessionID)
			}
		case "skip":
			fmt.Fprintf(cmd.OutOrStdout(), "  -> Skipped\n\n")
		}
	}

	if finalErr != nil {
		return finalErr
	}

	return nil
}

// classifySession determines if a session is stuck and returns diagnostic info.
// Returns nil if the session is healthy.
func classifySession(state *strategy.SessionState, repo *git.Repository, now time.Time) *stuckSession {
	// Determine shadow branch info
	shadowBranch := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, refErr := repo.Reference(refName, true)
	hasShadowBranch := refErr == nil

	stuck := func(reason string) *stuckSession {
		return &stuckSession{
			State:             state,
			Reason:            reason,
			ShadowBranch:      shadowBranch,
			HasShadowBranch:   hasShadowBranch,
			CheckpointCount:   state.StepCount + len(state.TaskRecords),
			FilesTouchedCount: len(state.FilesTouched),
		}
	}

	// A dead owner is unambiguous and phase-independent, so it is checked ahead
	// of the phase switch: an agent that quit without firing a session-end hook
	// leaves the session IDLE just as often as ACTIVE (see State.OwnerExited).
	// Detected immediately, with no timeout wait. These are normally finalized
	// up front in runSessionsFix; this covers sessions that couldn't be.
	if state.OwnerExited() {
		pid := 0
		if state.Owner != nil {
			pid = state.Owner.PID
		}
		return stuck(fmt.Sprintf("agent process %d exited (no longer running)", pid))
	}

	switch {
	case state.Phase.IsActive():
		switch {
		case !state.IsStuckActive():
			return nil
		case state.LastInteractionTime != nil:
			return stuck(fmt.Sprintf("active, last interaction %s ago", now.Sub(*state.LastInteractionTime).Truncate(time.Minute)))
		default:
			return stuck(fmt.Sprintf("active, started %s ago with no recorded interaction", now.Sub(state.StartedAt).Truncate(time.Minute)))
		}

	case state.Phase == session.PhaseEnded:
		// FullyCondensed = everything worth keeping is materialized; a leftover
		// live record can never complete (owner gone) and must not re-flag forever.
		if state.FullyCondensed {
			return nil
		}
		// Task records never live on the shadow branch, so branch absence must not hide them.
		if state.HasTaskContent() {
			return stuck("ended with uncondensed checkpoint data")
		}
		if state.StepCount <= 0 || !hasShadowBranch {
			return nil
		}
		return stuck("ended with uncondensed checkpoint data")

	default:
		return nil
	}
}

// displayStuckSession prints diagnostic info for a stuck session.
func displayStuckSession(cmd *cobra.Command, ss stuckSession) {
	w := cmd.OutOrStdout()

	fmt.Fprintf(w, "  Session: %s\n", ss.State.SessionID)
	fmt.Fprintf(w, "  Phase:   %s\n", ss.State.Phase)
	fmt.Fprintf(w, "  Reason:  %s\n", ss.Reason)

	if ss.State.AgentType != "" {
		fmt.Fprintf(w, "  Agent:   %s\n", ss.State.AgentType)
	}

	if ss.State.LastInteractionTime != nil {
		fmt.Fprintf(w, "  Last interaction: %s\n", ss.State.LastInteractionTime.Format(time.RFC3339))
	}

	shadowStatus := "not found"
	if ss.HasShadowBranch {
		shadowStatus = fmt.Sprintf("exists (%s)", ss.ShadowBranch)
	}
	fmt.Fprintf(w, "  Shadow branch: %s\n", shadowStatus)
	fmt.Fprintf(w, "  Checkpoints: %d, Files touched: %d\n", ss.CheckpointCount, ss.FilesTouchedCount)
}

// canCondenseStuckSession reports whether a stuck session has content the
// condense path can save: shadow-branch checkpoints, or task records — which
// never live on the shadow branch, so a record-bearing dead-owner session
// must be condensed (materialized), never discarded.
func canCondenseStuckSession(ss stuckSession) bool {
	return (ss.HasShadowBranch && ss.CheckpointCount > 0) || ss.State.HasTaskContent()
}

// promptSessionAction asks the user what to do with a stuck session.
func promptSessionAction(ss stuckSession) (string, error) {
	var action string

	options := make([]huh.Option[string], 0, 3)
	if canCondenseStuckSession(ss) {
		options = append(options, huh.NewOption("Condense (save to permanent storage)", "condense"))
	}
	options = append(options,
		huh.NewOption("Discard (remove session data)", "discard"),
		huh.NewOption("Skip (leave as-is)", "skip"),
	)

	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Fix session %s?", ss.State.SessionID)).
				Options(options...).
				Value(&action),
		),
	)

	if err := form.Run(); err != nil {
		return "", fmt.Errorf("session fix prompt failed: %w", err)
	}

	return action, nil
}

// discardSession removes session state and cleans up the shadow branch.
func discardSession(ctx context.Context, ss stuckSession, _ *git.Repository, errW io.Writer) error {
	// Clear session state file
	if err := strategy.ClearSessionState(ctx, ss.State.SessionID); err != nil {
		return fmt.Errorf("failed to clear session state: %w", err)
	}

	// Delete shadow branch if it exists and no other sessions need it
	if ss.HasShadowBranch {
		if shouldDelete, err := canDeleteShadowBranch(ctx, ss.ShadowBranch, ss.State.SessionID); err != nil {
			fmt.Fprintf(errW, "Warning: could not check other sessions for shadow branch: %v\n", err)
		} else if shouldDelete {
			if err := strategy.DeleteBranchCLI(ctx, ss.ShadowBranch); err != nil {
				// Branch already gone is not an error — keeps discard idempotent
				if !errors.Is(err, strategy.ErrBranchNotFound) {
					return fmt.Errorf("failed to delete shadow branch: %w", err)
				}
			}
		}
	}

	return nil
}

// reportMetadataDivergence prints the metadata-branch verdict for a comparison
// that is not disconnected: either plain OK, or DIVERGED when both sides advanced
// since their merge base.
//
// Divergence needs saying because it is the one state whose resolution rewrites
// the local ref: the next fetch from the elected sync remote replays the local
// commits onto the fetched tip (SafelyAdvanceLocalRef), which loses no
// checkpoints but does move the ref and re-hash those commits. Nothing else
// surfaces that — the replay itself only logs.
//
// Takes the already-computed comparison rather than re-deriving it: the verdict
// costs a merge-base subprocess, and the caller has one in hand.
func reportMetadataDivergence(ctx context.Context, w io.Writer, comparison strategy.MetadataComparison, remoteName string, primary plumbing.ReferenceName) {
	if comparison.Relation != strategy.MetadataRelationDiverged {
		fmt.Fprintln(w, "✓ Metadata branches: OK")
		return
	}

	fmt.Fprintln(w, "Metadata branches: DIVERGED")
	fmt.Fprintf(w, "  Local and remote %s have both advanced since they last agreed.\n", primary.Short())
	// Labels are fixed-width and the remote name trails the hash: padding it
	// into the label misaligns the pair for every remote name that is not
	// exactly as long as "origin".
	fmt.Fprintf(w, "    local:  %s\n", comparison.Local.String()[:12])
	fmt.Fprintf(w, "    remote: %s  (%s)\n", comparison.Remote.String()[:12], remoteName)

	// Whether the divergence resolves itself depends on the confinement rule:
	// only the elected sync remote may advance the local ref, so a diverged
	// legacy-tier ref sits there indefinitely and says nothing about local state.
	elected, electErr := strategy.ResolveCheckpointSyncRemote(ctx)
	if electErr == nil && elected.Name == remoteName {
		fmt.Fprintln(w, "  No action needed: the next fetch from this remote replays your local")
		fmt.Fprintln(w, "  checkpoints onto its tip. No checkpoints are lost, but the local ref moves")
		fmt.Fprintln(w, "  and the replayed commits get new hashes.")
		return
	}
	fmt.Fprintf(w, "  %q is the legacy read tier, not the elected checkpoint sync remote, so it\n", remoteName)
	fmt.Fprintln(w, "  never advances the local ref. Nothing will reconcile this on its own.")
}

// checkDisconnectedMetadata detects and optionally repairs disconnected
// local/remote metadata branches (the "empty-orphan bug").
func checkDisconnectedMetadata(cmd *cobra.Command, force bool) error {
	repo, err := openRepository(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	ctx := cmd.Context()
	refs := checkpoint.ResolveRefs(ctx)
	w := cmd.OutOrStdout()
	if !refs.PrimaryFetchableFromRemote() {
		fmt.Fprintf(w, "✓ Metadata branches: OK (primary ref %s is not pushed to a remote)\n", refs.Primary)
		return nil
	}
	// Check against the first checkpoint read candidate whose remote-tracking
	// ref exists — a pure read across both tiers (elected sync remote, then
	// the legacy origin tier).
	remoteName, remoteRefName, ok := strategy.FirstReadCandidateTrackingRef(ctx, repo, refs.Primary)
	if !ok {
		fmt.Fprintln(w, "✓ Metadata branches: OK (no remote-tracking metadata ref found)")
		return nil
	}
	// One classification for both verdicts: disconnected and diverged are two
	// answers from the same merge base, and computing them separately meant two
	// merge-base subprocesses that could disagree.
	comparison, err := strategy.CompareMetadataWithRemote(ctx, repo, remoteRefName)
	if err != nil {
		return fmt.Errorf("could not check metadata branch state: %w", err)
	}

	if comparison.Relation != strategy.MetadataRelationDisconnected {
		reportMetadataDivergence(ctx, w, comparison, remoteName, refs.Primary)
		return nil
	}

	fmt.Fprintln(w, "Metadata branches: DISCONNECTED")
	fmt.Fprintf(w, "  Local and remote %s branches share no common ancestor.\n", refs.Primary.Short())
	fmt.Fprintln(w, "  Some remote checkpoints may not be visible locally.")

	// The repair advances the local ref from the remote tip, so it is
	// confined to the elected checkpoint sync remote: a stale legacy-tier
	// origin must never drive a local-ref rewrite. Report-only otherwise.
	elected, electErr := strategy.ResolveCheckpointSyncRemote(ctx)
	if electErr != nil || elected.Name != remoteName {
		fmt.Fprintf(w, "  The disconnected tracking ref belongs to remote %q, which is not the elected checkpoint sync remote.\n", remoteName)
		fmt.Fprintln(w, "  Automatic repair only reconciles against the elected sync remote; fetch its metadata branch and re-run 'entire doctor'.")
		return nil
	}

	fmt.Fprintln(w, "  Fix: cherry-pick local checkpoints onto remote tip (preserves all data).")

	if !force {
		// Degrade to warn-only when there is nobody to ask, same as
		// checkGitHooks: an agent or CI run must not crash on a TTY prompt.
		if !interactive.CanPromptInteractively() {
			fmt.Fprintln(w, "  Run `entire doctor --force` to apply it.")
			return nil
		}
		proceed, promptErr := confirmDoctorFix(ctx, w, "Fix disconnected metadata branches?")
		if promptErr != nil {
			return promptErr
		}
		if !proceed {
			return nil
		}
	}

	if fixErr := strategy.ReconcileDisconnectedMetadataRef(ctx, repo, refs.Primary, remoteRefName, cmd.ErrOrStderr()); fixErr != nil {
		return fmt.Errorf("failed to reconcile metadata branches: %w", fixErr)
	}

	fmt.Fprintln(w, "  ✓ Fixed: metadata branches reconciled")
	return nil
}

// confirmDoctorFix prompts to apply a doctor fix. Declining (which prints
// "-> Skipped"), aborting (Ctrl+C), context cancellation, and a
// non-interactive environment all return false with no error. Callers that
// want to hint at `--force` for the non-interactive case should check
// interactive.CanPromptInteractively themselves before calling; the guard
// here is the safety net so no future call site can crash on a TTY prompt.
func confirmDoctorFix(ctx context.Context, w io.Writer, title string) (bool, error) {
	// huh opens the TTY during form startup regardless of context state, so
	// guard explicitly to honor an already-cancelled command context.
	if ctx.Err() != nil {
		return false, nil //nolint:nilerr // cancelled context is a clean skip, not an error
	}
	// Never open the TTY prompt when there is nobody to ask (agents, CI):
	// decline silently instead of crashing with "could not open TTY".
	if !interactive.CanPromptInteractively() {
		return false, nil
	}
	var confirmed bool
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(title).
				Value(&confirmed),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, fmt.Errorf("prompt failed: %w", err)
	}
	if !confirmed {
		fmt.Fprintln(w, "  -> Skipped")
	}
	return confirmed, nil
}

// checkGitHooks reports whether Entire's git hooks are installed and current,
// and offers to reinstall them when they are not.
//
// This is the one hook surface a user cannot see going wrong. Agent hook configs
// are committed files that turn up in a diff; .git/hooks is per-clone and
// untracked, so pulling a release that changes hook generation never touches it.
// A hook left by the removed local-dev mode still carries Entire's marker, so it
// looks installed while naming a launcher inside the working tree that no longer
// exists — and because that shape got no availability guard while pre-push
// deliberately propagates exit codes, the symptom a user actually sees is `git
// push` being rejected.
//
// Unlike the agent hook checks this one repairs rather than only warning: the
// reinstall is content-idempotent, backs up any foreign hook it displaces, and
// writes exactly what the next turn-start would write anyway. Someone reaching
// for doctor after a rejected push wants to be unblocked, not handed a second
// command to run.
func checkGitHooks(cmd *cobra.Command, force bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	migrateAbsoluteGitHookPath := func() {
		// Reported alongside a healthy result too: the hooks are fine today and
		// will stop being fine on upgrade, which is exactly when a warning is
		// worth reading. Not reported in a repository that never opted into
		// Entire, where the whole check stays silent.
		s, err := settings.Load(ctx)
		if err != nil || s.AbsoluteGitHookPathDeprecation() == "" {
			return
		}
		fmt.Fprintln(w, "absolute_git_hook_path: COMMITTED")
		fmt.Fprintf(w, "  %s\n", s.AbsoluteGitHookPathDeprecation())
		fmt.Fprintf(w, "  Fix: copy it to %s. Your hooks keep working exactly as they do now.\n",
			settings.EntireSettingsLocalFile)

		if !force {
			// Same degradation as the hook repair below: an agent or CI run must
			// not block on a prompt, and must not edit settings unasked.
			if !interactive.CanPromptInteractively() {
				fmt.Fprintln(w, "  Run `entire doctor --force` to apply it.")
				return
			}
			proceed, promptErr := confirmDoctorFix(ctx, w, "Copy absolute_git_hook_path to local settings?")
			if promptErr != nil || !proceed {
				return
			}
		}

		// Copy, not move: the committed key is left alone. Editing a tracked file
		// would put an unexpected change in the user's next commit, and once the
		// local file sets the key the committed one is redundant — it stops being
		// honored on upgrade without anything else happening.
		if err := setAbsoluteGitHookPathLocal(ctx, true); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  could not write %s: %v\n", settings.EntireSettingsLocalFile, err)
			return
		}
		fmt.Fprintf(w, "  ✓ Copied to %s (the committed value is now redundant and can be removed at your leisure)\n",
			settings.EntireSettingsLocalFile)
	}

	state, outdatedReason := strategy.CheckGitHookState(ctx)
	switch state {
	case strategy.GitHooksCurrent:
		migrateAbsoluteGitHookPath()
		fmt.Fprintln(w, "✓ Git hooks: OK")
		return nil

	case strategy.GitHooksAbsent:
		// Missing hooks are only a problem where Entire was actually set up.
		// doctor runs in any git repo, so treating this as actionable would let
		// `entire doctor --force` install hooks into — and back up the existing
		// hooks of — a repo that never opted in. That is the loudest possible
		// surprise from a diagnostic command. The sibling checks already hold this
		// line: checkHookDrift stays silent on HooksAbsent, and
		// checkCodexHookTrust returns early when there is no codex hooks file.
		//
		// IsSetUpAny rather than IsSetUpAndEnabled: a repo that ran `entire
		// disable` still wants working hooks, because the hooks themselves are
		// what no-op while disabled.
		if !settings.IsSetUpAny(ctx) {
			return nil
		}
		migrateAbsoluteGitHookPath()
		fmt.Fprintln(w, "Git hooks: NOT INSTALLED")
		fmt.Fprintln(w, "  Commits in this repository are not captured as checkpoints.")

	case strategy.GitHooksOutdated:
		// Actionable whatever the settings say: a hook carrying Entire's marker
		// means this repo opted in at some point, and a stale one is actively
		// broken rather than merely missing. The reason comes from the check
		// because the causes need different advice.
		migrateAbsoluteGitHookPath()
		fmt.Fprintln(w, "Git hooks: OUT OF DATE")
		fmt.Fprintf(w, "  %s\n", outdatedReason)
	}
	fmt.Fprintln(w, "  Fix: reinstall the managed git hooks (any non-Entire hook is backed up).")

	if !force {
		// Degrade to warn-only when there is nobody to ask. An agent or CI run
		// must not be blocked on a prompt, and rewriting a repo's hooks
		// unasked is worse than naming the command that does it.
		if !interactive.CanPromptInteractively() {
			fmt.Fprintln(w, "  Run `entire doctor --force` to apply it.")
			return nil
		}
		proceed, promptErr := confirmDoctorFix(ctx, w, "Reinstall Entire git hooks?")
		if promptErr != nil {
			return promptErr
		}
		if !proceed {
			return nil
		}
	}

	if _, err := strategy.ReinstallGitHooks(ctx); err != nil {
		return fmt.Errorf("failed to reinstall git hooks: %w", err)
	}
	fmt.Fprintln(w, "  ✓ Fixed: git hooks reinstalled")
	return nil
}

// checkLogSink reports a .entire/logs Entire cannot write to.
//
// Every other diagnostic in the CLI is delivered by writing there, and that
// write is deliberately silent about its own failure — a dropped log line must
// never surface as an error in the caller. So an unwritable log directory
// presents exactly like a repo where nothing ever ran: no message, exit 0, and
// an absent or empty entire.log. That is the shape of the support report this
// logging work exists to fix, so doctor is the wrong command to reproduce it.
//
// Read-only. The fixes are ownership and permissions, which doctor cannot take
// on the user's behalf.
//
// A nil logger means the entry point declined to build one, i.e. Entire was
// never set up here — nothing to nag about, and reading it back from the
// context is what keeps this check on the same directory the logger actually
// uses instead of re-deriving the path.
func checkLogSink(cmd *cobra.Command) {
	err := logging.LoggerFromContext(cmd.Context()).EnsureOpen()
	if err == nil {
		return
	}

	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "Operational logs: NOT WRITABLE")
	fmt.Fprintf(w, "  %v\n", err)
	fmt.Fprintf(w, "  Entire's diagnostics are being dropped, including the redaction warnings\n")
	fmt.Fprintf(w, "  that explain why a custom rule isn't matching. `entire doctor logs` and\n")
	fmt.Fprintf(w, "  `entire doctor bundle` have nothing to report until this is fixed.\n")
	fmt.Fprintf(w, "  Fix: resolve the error above so %s is a writable directory — commonly\n", logging.LogsDir)
	fmt.Fprintln(w, "  ownership or permissions, a regular file occupying the path, or a full disk.")
}

// checkHookDrift warns when an installed agent's Entire hook config is out of
// date — an older release wrote Claude Code tool matchers that no longer fire,
// or a repo committed a Pi/OpenCode extension that the template has since moved
// past. Read-only; the fix is `entire enable --force`. Stays silent for agents
// that aren't installed here or don't implement a drift check.
func checkHookDrift(cmd *cobra.Command) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	for _, name := range GetAgentsWithHooksInstalled(ctx) {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if _, ownsDiagnostics := agent.AsEffectiveHookDiagnostics(ag); ownsDiagnostics {
			continue
		}
		hf, ok := agent.AsHookFreshness(ag)
		if !ok {
			continue
		}
		displayName := string(ag.Type())
		switch hf.CheckHookConfig(ctx) {
		case agent.HooksAbsent:
			// Not installed in this repo — nothing to report.
		case agent.HooksCurrent:
			fmt.Fprintf(w, "✓ %s hook config: OK\n", displayName)
		case agent.HooksOutdated:
			fmt.Fprintf(w, "%s hooks: OUT OF DATE\n", displayName)
			fmt.Fprintln(w, "  The installed hook config no longer matches what this CLI writes,")
			fmt.Fprintln(w, "  so some or all hooks may silently not fire.")
			fmt.Fprintln(w, "  Run `entire enable --force` to update it.")
		}
	}
}

// checkCodexHookTrust reports whether Codex can discover its effective
// hooks file, whether its Entire-managed event set is current, and whether the
// local Codex config has approval records for every declared hook. All checks
// are structural; Entire never computes or copies Codex trust hashes.
func checkCodexHookTrust(cmd *cobra.Command) {
	diagnostics := codex.InspectHookDiagnostics(cmd.Context())
	w := cmd.OutOrStdout()
	issue := codexHookIssueFromDiagnostics(diagnostics)
	if issue == nil {
		if diagnostics.Discovered.State == codex.HookFileEntire && diagnostics.Discovery.ProjectLayerExists() {
			writeCodexInstalledAndTrust(w, diagnostics)
		}
		return
	}

	worktreePath := diagnostics.WorktreeHooks.Path()
	discoveredPath := diagnostics.Discovery.DiscoveredHooks.Path()
	switch issue.State {
	case codexHookStateDiscoveryUnresolved:
		fmt.Fprintln(w, "Codex hooks: UNRESOLVED")
		if worktreePath != "" {
			fmt.Fprintf(w, "  Current-worktree hooks: %s\n", worktreePath)
		}
		fmt.Fprintf(w, "  Entire could not resolve the hooks file Codex discovers: %v\n", diagnostics.Discovery.Diagnostic)
		fmt.Fprintln(w, "  Inspect the Git layout manually; Entire will not guess or write to another checkout.")
	case codexHookStateMalformedDiscovered, codexHookStateUnavailableDiscovered:
		writeCodexDiscoveredInspectionWarning(w, discoveredPath, diagnostics.Discovered.State, diagnostics.Discovered.Err)
	case codexHookStateProjectLayerMissing:
		writeCodexMissingProjectLayerWarning(w, filepath.Dir(worktreePath), discoveredPath)
	case codexHookStateInactiveWorktreePath:
		writeCodexInactiveWorktreeWarning(w, worktreePath, discoveredPath)
	case codexHookStateWorktreePathNotDiscovered:
		writeCodexActiveViaRoot(w, diagnostics)
	case codexHookStateMalformedWorktree, codexHookStateUnavailableWorktree:
		writeCodexWorktreeInspectionWarning(w, worktreePath, diagnostics.Worktree.State, diagnostics.Worktree.Err)
	case codexHookStateOutdated:
		writeCodexInstalledAndTrust(w, diagnostics)
		fmt.Fprintln(w, "Codex hooks: OUT OF DATE")
		fmt.Fprintf(w, "  Codex-discovered hooks: %s\n", discoveredPath)
		if len(diagnostics.Discovered.Missing) > 0 {
			fmt.Fprintf(w, "  %d hook(s) the CLI installs today aren't declared there:\n", len(diagnostics.Discovered.Missing))
			for _, ev := range diagnostics.Discovered.Missing {
				fmt.Fprintf(w, "    - %s\n", ev)
			}
		} else {
			fmt.Fprintln(w, "  Entire-managed commands or timeouts there do not match this CLI.")
		}
		if diagnostics.PathsDiffer() {
			writeCodexPrimaryCheckoutRemedy(w)
		} else {
			fmt.Fprintln(w, "  Run `entire enable --force` from this worktree to refresh it.")
		}
	case codexHookStateTrustReview:
		writeCodexInstalledAndTrust(w, diagnostics)
	}
}

func writeCodexInstalledAndTrust(w io.Writer, diagnostics codex.HookDiagnostics) {
	writeCodexHookStatus(w, diagnostics, false)
}

func writeCodexActiveViaRoot(w io.Writer, diagnostics codex.HookDiagnostics) {
	writeCodexHookStatus(w, diagnostics, true)
}

func writeCodexHookStatus(w io.Writer, diagnostics codex.HookDiagnostics, activeViaRoot bool) {
	if diagnostics.Discovered.CoreInstalled {
		if activeViaRoot {
			fmt.Fprintln(w, "✓ Codex hooks: ACTIVE (via root checkout)")
		} else {
			fmt.Fprintln(w, "✓ Codex hooks: INSTALLED")
		}
		fmt.Fprintf(w, "  Codex-discovered hooks: %s\n", diagnostics.Discovery.DiscoveredHooks.Path())
	}
	switch {
	case len(diagnostics.Trust.Declared) > 0 && !diagnostics.Trust.Known:
		fmt.Fprintln(w, "Codex hook trust: UNKNOWN")
		fmt.Fprintln(w, "  The hooks are installed, but Codex's local approval records could not be read.")
		fmt.Fprintln(w, "  Open /hooks inside Codex to review their active state.")
	case len(diagnostics.Trust.Gaps) > 0:
		fmt.Fprintln(w, "Codex hook trust: REVIEW NEEDED")
		fmt.Fprintf(w, "  %d installed hook(s) have no approval record at the Codex-discovered path:\n", len(diagnostics.Trust.Gaps))
		for _, ev := range diagnostics.Trust.Gaps {
			fmt.Fprintf(w, "    - %s\n", ev)
		}
		fmt.Fprintln(w, "  Open /hooks inside Codex to approve them.")
	case len(diagnostics.Trust.Declared) > 0:
		fmt.Fprintln(w, "✓ Codex hook approval records: PRESENT")
	}
}

func writeCodexInactiveWorktreeWarning(w io.Writer, worktreePath, discoveredPath string) {
	fmt.Fprintln(w, "Codex hooks: NOT ACTIVE IN THIS WORKTREE")
	fmt.Fprintln(w, "  Entire hooks are configured at the current-worktree path:")
	fmt.Fprintf(w, "    %s\n", worktreePath)
	fmt.Fprintln(w, "  Codex currently discovers:")
	fmt.Fprintf(w, "    %s\n", discoveredPath)
	writeCodexPrimaryCheckoutRemedy(w)
}

func writeCodexWorktreeInspectionWarning(w io.Writer, worktreePath string, state codex.HookFileState, err error) {
	if state == codex.HookFileMalformed {
		fmt.Fprintln(w, "Codex hooks: MALFORMED CURRENT-WORKTREE CONFIGURATION")
	} else {
		fmt.Fprintln(w, "Codex hooks: CURRENT-WORKTREE CONFIGURATION UNAVAILABLE")
	}
	if worktreePath != "" {
		fmt.Fprintf(w, "  Current-worktree hooks: %s\n", worktreePath)
	}
	fmt.Fprintf(w, "  Error: %v\n", err)
	fmt.Fprintln(w, "  Fix the current-worktree .codex path or hooks.json file, then run `entire enable --force`.")
	fmt.Fprintln(w, "  This may not be the file Codex reads. If Codex discovers another project root, apply/merge the generated .codex/hooks.json change there too.")
	fmt.Fprintln(w, "  .codex/hooks.json is tracked — commit it and make sure the discovered project root has it too.")
}

func writeCodexDiscoveredInspectionWarning(w io.Writer, discoveredPath string, state codex.HookFileState, err error) {
	if state == codex.HookFileMalformed {
		fmt.Fprintln(w, "Codex hooks: MALFORMED DISCOVERED CONFIGURATION")
	} else {
		fmt.Fprintln(w, "Codex hooks: DISCOVERED CONFIGURATION UNAVAILABLE")
	}
	fmt.Fprintf(w, "  Codex-discovered hooks: %s\n", discoveredPath)
	fmt.Fprintf(w, "  Error: %v\n", err)
	fmt.Fprintln(w, "  Fix this discovered .codex/hooks.json file in its project root.")
	writeCodexTrackedHooksRemedy(w)
}

func writeCodexMissingProjectLayerWarning(w io.Writer, projectLayerPath, discoveredPath string) {
	fmt.Fprintln(w, "Codex hooks: PROJECT LAYER MISSING")
	fmt.Fprintf(w, "  Current-worktree project layer: %s (missing)\n", projectLayerPath)
	fmt.Fprintf(w, "  Codex-discovered hooks: %s\n", discoveredPath)
	fmt.Fprintln(w, "  Current Codex needs the local .codex project layer before it loads the discovered file.")
	fmt.Fprintln(w, "  Run `entire enable` from this worktree to create the local layer.")
	writeCodexTrackedHooksRemedy(w)
}

func writeCodexPrimaryCheckoutRemedy(w io.Writer) {
	fmt.Fprintln(w, "  Codex will read the discovered file above, not the current-worktree file above.")
	writeCodexTrackedHooksRemedy(w)
}

func writeCodexTrackedHooksRemedy(w io.Writer) {
	fmt.Fprintln(w, "  .codex/hooks.json is tracked — commit it and make sure the root worktree has it")
	fmt.Fprintln(w, "  (merge to the default branch, or check that branch out there).")
}

// canDeleteShadowBranch checks if a shadow branch can be safely deleted.
// Returns true if no other sessions (besides excludeSessionID) need this branch.
func canDeleteShadowBranch(ctx context.Context, shadowBranch, excludeSessionID string) (bool, error) {
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to list session states: %w", err)
	}

	for _, state := range states {
		if state.SessionID == excludeSessionID {
			continue
		}
		// Task records never live on the shadow branch, so only SaveStep
		// checkpoints pin it alive.
		otherShadow := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		if otherShadow == shadowBranch && state.StepCount > 0 {
			return false, nil
		}
	}

	return true, nil
}
