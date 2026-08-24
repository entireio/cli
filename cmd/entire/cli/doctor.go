package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

	// Global tracking tier: user-level agent hook coverage and this clone's
	// lazy-setup state.
	checkGlobalTracking(cmd)

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

	// Finalize any ACTIVE session whose agent process has exited (no SessionStop
	// hook fired). A gone process is unambiguous, so these are condensed on the
	// spot rather than left for the interactive prompt below; the sweep marks
	// them ended in place so classifySession won't re-flag them.
	if n := finalizeExitedSessions(ctx, states); n > 0 {
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

	switch strategy.CheckGitHookState(ctx) {
	case strategy.GitHooksCurrent:
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
		fmt.Fprintln(w, "Git hooks: NOT INSTALLED")
		fmt.Fprintln(w, "  Commits in this repository are not captured as checkpoints.")

	case strategy.GitHooksOutdated:
		// Actionable whatever the settings say: a hook carrying Entire's marker
		// means this repo opted in at some point, and a stale one is actively
		// broken rather than merely missing.
		fmt.Fprintln(w, "Git hooks: OUT OF DATE")
		fmt.Fprintln(w, "  A hook still runs Entire from the working tree instead of the installed")
		fmt.Fprintln(w, "  binary. This can reject `git push`, because the path it names is gone.")
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

// checkCodexHookTrust warns about two kinds of drift in the Codex hook
// setup:
//
//  1. .codex/hooks.json is stale relative to what the CLI installs
//     today (e.g. a release added PostToolUse after the user enabled
//     Codex). Fix: re-run `entire enable`.
//
//  2. A declared hook lacks a `trusted_hash` entry in the user's Codex
//     config — either a fresh clone or a newer hook on the file the
//     user hasn't approved yet. Fix: open /hooks in Codex.
//
// Both checks are structural (file/key presence). Stays silent when
// this repo doesn't have codex hooks installed or when we can't
// resolve the worktree root. Warn-only.
func checkCodexHookTrust(cmd *cobra.Command) {
	repoRoot, err := paths.WorktreeRoot(cmd.Context())
	if err != nil {
		return
	}
	if _, statErr := os.Stat(filepath.Join(repoRoot, ".codex", "hooks.json")); statErr != nil {
		return
	}

	w := cmd.OutOrStdout()
	missing := codex.MissingEntireHooks(repoRoot)
	gaps := codex.HookTrustGaps(repoRoot)

	if len(missing) == 0 && len(gaps) == 0 {
		fmt.Fprintln(w, "✓ Codex hook trust: OK")
		return
	}

	if len(missing) > 0 {
		fmt.Fprintln(w, "Codex hooks: OUT OF DATE")
		fmt.Fprintf(w, "  %d hook(s) the CLI installs today aren't declared in .codex/hooks.json:\n", len(missing))
		for _, ev := range missing {
			fmt.Fprintf(w, "    - %s\n", ev)
		}
		fmt.Fprintln(w, "  Run `entire enable` to refresh the hooks file.")
	}

	if len(gaps) > 0 {
		fmt.Fprintln(w, "Codex hook trust: REVIEW NEEDED")
		fmt.Fprintf(w, "  %d hook(s) declared in .codex/hooks.json have no trusted_hash entry yet:\n", len(gaps))
		for _, ev := range gaps {
			fmt.Fprintf(w, "    - %s\n", ev)
		}
		fmt.Fprintln(w, "  Open /hooks inside Codex to approve them.")
	}
}

// checkGlobalTracking runs the global-mode diagnostics. Read-only except for
// check 5's drift shape, which clears the clone's global_setup_completed
// marker (the marker's documented drift contract):
//
//  1. The user-global settings file exists but cannot be read or parsed —
//     global tracking is silently off machine-wide (fail closed). This is
//     the one failure hook Debug logs can never surface: on the hook paths
//     logging.Init runs only after the gate that reads this file has already
//     failed.
//  2. Global tracking is on but user-level agent hooks are missing for an
//     agent that supports them — sessions in never-enabled repos cannot fire
//     a hook for those agents. Fix: `entire enable --global` (idempotent)
//     re-installs them. An agent whose user-level config cannot be READ gets
//     its own warning: it is unverified, not missing, and the missing-hooks
//     remedy would refuse to run against the broken file anyway.
//  3. Unusable exclude patterns (relative, unsupported ~user form, invalid
//     glob) — under the fail-closed rule each one deactivates the tier in
//     every repo it is checked against.
//  4. exclude_origins is configured and this repo's origin is present but
//     cannot be normalized to host/owner/repo — informational: the tier
//     stays off in this repo (fail closed).
//  5. This clone carries the global_setup_completed clone-preferences marker
//     but its git hooks are gone. Two shapes: when core.hooksPath resolves
//     inside the worktree the absence is deliberate (the lazy setup skips
//     worktree writes and still sets the marker), so doctor explains that
//     hook capture requires repo-level enable and leaves the marker alone.
//     Otherwise it is drift — the marker is a run-once latch, so the lazy
//     setup would never revisit it; per the marker's contract (any component
//     detecting drift must clear it) doctor clears it so the next hook
//     activity re-runs the lazy setup. A residency probe that ERRORS is its
//     own warn with no marker mutation: the same probe error makes the lazy
//     setup skip hook installation, so clearing would promise a reinstall
//     that never happens.
//
// All checks except 1 stay silent while the global tier is unconfigured or off.
func checkGlobalTracking(cmd *cobra.Command) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		fmt.Fprintln(w, "Global tracking: USER SETTINGS UNREADABLE")
		fmt.Fprintf(w, "  %s cannot be read or parsed: %v\n", settings.UserSettingsPath(), err)
		fmt.Fprintln(w, "  Global tracking is silently off machine-wide (fail closed), and hook debug logs")
		fmt.Fprintln(w, "  cannot report it: hook logging starts only after this file has gated the hook off.")
		fmt.Fprintln(w, "  Fix the JSON by hand or remove the file (an unknown key means a newer entire wrote it — upgrade instead).")
		return
	}
	if !us.GlobalEnabled() {
		return
	}

	var missing, unverifiable []string
	supports, _ := agent.UserHookSupports()
	for _, ua := range supports {
		installed, hookErr := ua.Support.AreUserHooksInstalled(ctx)
		switch {
		case hookErr != nil:
			unverifiable = append(unverifiable, fmt.Sprintf("%s (%v)", ua.Name, hookErr))
		case !installed:
			missing = append(missing, string(ua.Name))
		}
	}
	if len(missing) == 0 && len(unverifiable) == 0 {
		fmt.Fprintln(w, "✓ Global tracking: user-level agent hooks OK")
	}
	if len(missing) > 0 {
		fmt.Fprintln(w, "Global tracking: USER-LEVEL AGENT HOOKS MISSING")
		fmt.Fprintf(w, "  Global tracking is on, but user-level hooks are not installed for: %s\n", strings.Join(missing, ", "))
		fmt.Fprintln(w, "  Sessions in repos without repo-level setup are not tracked for those agents.")
		fmt.Fprintln(w, "  Run `entire enable --global` to install them.")
	}
	if len(unverifiable) > 0 {
		fmt.Fprintln(w, "Global tracking: USER-LEVEL AGENT HOOKS UNVERIFIABLE")
		fmt.Fprintln(w, "  Could not read the user-level hook config for:")
		for _, u := range unverifiable {
			fmt.Fprintf(w, "    - %s\n", u)
		}
		fmt.Fprintln(w, "  Fix the named files, then run `entire enable --global` to (re)install the hooks.")
	}

	// us was loaded (and the tier confirmed enabled) at the top of this
	// function; the pure validators take it directly instead of re-reading
	// the settings file per check.
	if problems := settings.ValidateGlobalPatterns(us.Global); len(problems) > 0 {
		fmt.Fprintln(w, "Global tracking: UNUSABLE SETTINGS ENTRIES")
		fmt.Fprintln(w, "  Exclude entries fail closed: each deactivates global tracking in every repo it is checked against.")
		fmt.Fprintln(w, "  trusted_paths entries are skipped: an unusable one never grants checkpoint sync.")
		for _, p := range problems {
			fmt.Fprintf(w, "    - %s\n", p)
		}
		fmt.Fprintf(w, "  Fix the listed entries in %s: use absolute or ~/ paths and valid doublestar globs.\n", settings.UserSettingsPath())
	}

	if bad := settings.UnnormalizableOrigins(ctx, us.Global); len(bad) > 0 {
		fmt.Fprintln(w, "Global tracking: origin not checkable in this repo (informational)")
		fmt.Fprintln(w, "  exclude_origins is configured, but this repo's origin cannot be normalized to host/owner/repo:")
		for _, o := range bad {
			fmt.Fprintf(w, "    - %s\n", o)
		}
		fmt.Fprintln(w, "  Global tracking stays off in this repo (fail closed).")
	}

	// Untrusted enrolled repo: informational, never a failure — the hold is
	// the intended state until the user opts in.
	if settings.RepoUntrustedEnrolled(ctx) {
		fmt.Fprintln(w, "Global tracking: checkpoint sync held in this repo (informational)")
		switch n := heldCheckpointCount(ctx); {
		case n == 1:
			fmt.Fprintln(w, "  This repo is enrolled but not trusted; 1 checkpoint is held locally.")
		case n > 1:
			fmt.Fprintf(w, "  This repo is enrolled but not trusted; %d checkpoints are held locally.\n", n)
		default:
			fmt.Fprintln(w, "  This repo is enrolled but not trusted.")
		}
		fmt.Fprintln(w, "  This is intended until you opt in; run `entire trust` to sync.")
	}

	// Clone-local check: only meaningful when the lazy setup already ran here.
	prefs, prefsErr := settings.LoadClonePreferences(ctx)
	if prefsErr != nil || !prefs.GlobalSetupCompleted {
		return
	}
	if !strategy.IsGitHookInstalled(ctx) {
		// A worktree-resident core.hooksPath (e.g. a committed .husky dir) is
		// the one shape where missing hooks are NOT drift: the lazy setup
		// deliberately skipped installation there (a worktree write would
		// break invisibility) and still set the marker. Explain instead of
		// clearing — clearing would just re-run a setup that skips again and
		// promise a reinstall that never happens.
		resident, hooksDir, resErr := strategy.HooksDirIsWorktreeResident(ctx)
		if resErr != nil {
			// A probe ERROR is neither shape: the lazy setup treats the same
			// error as "skip hook installation", so falling into the drift
			// branch would clear the marker and promise a reinstall that never
			// happens — an infinite mis-advice loop. Report, mutate nothing.
			fmt.Fprintln(w, "Globally tracked clone: GIT HOOK STATE UNVERIFIED")
			fmt.Fprintf(w, "  Could not resolve this clone's hooks directory: %v\n", resErr)
			fmt.Fprintln(w, "  Git hooks appear missing but the cause could not be determined; nothing was changed.")
			return
		}
		if resident {
			fmt.Fprintln(w, "Globally tracked clone: GIT HOOKS SKIPPED (core.hooksPath inside the worktree)")
			fmt.Fprintf(w, "  core.hooksPath resolves to %s, inside this worktree; global tracking never\n", hooksDir)
			fmt.Fprintln(w, "  writes worktree files, so its git hooks were deliberately not installed.")
			fmt.Fprintln(w, "  Agent-side session capture still works; commit-time checkpoint trailers do not.")
			fmt.Fprintln(w, "  For hook capture, enable Entire in this repo: 'entire enable' (repo-level setup")
			fmt.Fprintln(w, "  chains into an existing hooks dir), or point core.hooksPath back at .git/hooks.")
			return
		}
		fmt.Fprintln(w, "Globally tracked clone: GIT HOOKS MISSING")
		fmt.Fprintln(w, "  This clone was enabled by global tracking but its git hooks are gone.")
		// The global_setup_completed marker is a run-once latch: MaybeEnsureGlobalSetup
		// returns on it before ever checking the hooks, so a latched clone with
		// missing hooks never converges on its own. Clear the marker (the marker's
		// documented drift contract) so the next hook activity re-runs the setup.
		if clearErr := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
			p.GlobalSetupCompleted = false
			return nil
		}); clearErr != nil {
			fmt.Fprintf(w, "  Could not clear the clone's global_setup_completed marker (%v).\n", clearErr)
			fmt.Fprintln(w, "  Run `entire enable` here, or clear the marker manually, to restore tracking.")
			return
		}
		fmt.Fprintln(w, "  Marker cleared — the next hook activity in this repo re-runs the lazy setup and reinstalls them.")
	}
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
