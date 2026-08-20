package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
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

  When Codex hooks are installed:
  2. Codex hook trust: warn when hooks declared in .codex/hooks.json
     lack a trusted_hash entry in the user's Codex config (i.e. /hooks
     review hasn't run yet on this machine, or a newer entire release
     added a hook the user hasn't approved yet).

  For each installed agent that reports hook-config drift:
  3. Hook config: warn when the installed hooks no longer match what this
     CLI writes (e.g. an older release wrote Claude Code tool matchers that
     no longer fire, or a committed Pi/OpenCode extension has gone stale).
     Fix by re-running 'entire enable --force'.

  4. Stuck sessions: sessions stuck in ACTIVE or ENDED phase that need cleanup.

A session is considered stuck if:
  - It is in ACTIVE phase with no interaction for over 1 hour
  - It is in ENDED phase with uncondensed checkpoint data on a shadow branch

For each stuck session, you can choose to:
  - Condense: Save session data to permanent storage
  - Discard: Remove the session state and shadow branch data
  - Skip: Leave the session as-is

Use --force to condense all fixable sessions without prompting.  Sessions that can't
be condensed will be discarded.`,
		PreRun: func(_ *cobra.Command, _ []string) {
			strategy.EnsureRedactionConfigured()
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

	// Agent-specific: Codex hook trust state.
	checkCodexHookTrust(cmd)

	// Agent-specific: Cursor IDE on Windows opening this WSL repo over UNC
	// (\\wsl$), which runs no hooks at all.
	checkCursorUNCMode(cmd)

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

	// Route logging to .entire/logs/ for the rest of the command. Doctor emits
	// none itself, but the sweep below and the condense/discard handlers further
	// down all do, and with no logger installed those land on the user's terminal
	// via slog.Default(), interleaved with doctor's own report. It belongs here
	// rather than inside the sweep: the sweep returns early — without touching
	// logging — whenever no session needs finalizing, which is the common case
	// for a run that still has time-based stuck sessions to condense.
	defer ensureCommandLogging(ctx)()

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

	for _, ss := range stuck {
		displayStuckSession(cmd, ss)

		if force {
			if ss.HasShadowBranch && ss.CheckpointCount > 0 {
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
			CheckpointCount:   state.StepCount,
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
		// Ended sessions are stuck if they have uncondensed data
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

// promptSessionAction asks the user what to do with a stuck session.
func promptSessionAction(ss stuckSession) (string, error) {
	var action string

	options := make([]huh.Option[string], 0, 3)
	if ss.HasShadowBranch && ss.CheckpointCount > 0 {
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
// "-> Skipped"), aborting (Ctrl+C), and context cancellation all return false
// with no error.
func confirmDoctorFix(ctx context.Context, w io.Writer, title string) (bool, error) {
	// huh opens the TTY during form startup regardless of context state, so
	// guard explicitly to honor an already-cancelled command context.
	if ctx.Err() != nil {
		return false, nil //nolint:nilerr // cancelled context is a clean skip, not an error
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

// cursorWindowsUsersRoot is the Windows user-profile root as seen from WSL.
// Package var so tests can override it. Assumes Windows is on C: with the
// default WSL automount; other drive-letter mappings of \\wsl$ are missed.
var cursorWindowsUsersRoot = "/mnt/c/Users"

// checkCursorUNCMode warns when Cursor IDE on the Windows host has opened
// this WSL repo via a \\wsl$ UNC path: Cursor executes no hooks in that mode
// (see cursor/AGENT.md § Windows + WSL for the verified behavior), so
// sessions silently never track. Detection is the Windows-side project-dir
// fingerprint (cursor.DetectUNCProjectDirs). Stays silent outside WSL, when
// cursor hooks aren't installed in this repo, or when nothing matches.
// Warn-only: there is no CLI-side fix — the user must reopen the folder in
// WSL mode.
func checkCursorUNCMode(cmd *cobra.Command) {
	distro := os.Getenv("WSL_DISTRO_NAME")
	if distro == "" {
		return
	}
	ctx := cmd.Context()
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	ca := &cursor.CursorAgent{}
	if !ca.AreHooksInstalled(ctx) {
		return
	}
	matches := cursor.DetectUNCProjectDirs(cursorWindowsUsersRoot, distro, repoRoot, time.Now())
	if len(matches) == 0 {
		return
	}
	logging.Debug(ctx, "doctor: cursor UNC-mode fingerprint matched", "count", len(matches))
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "Cursor IDE hooks: NOT FIRING (recent sessions ran over \\\\wsl$)")
	fmt.Fprintln(w, "  Sessions opened this way are never tracked.")
	fmt.Fprintln(w, "  Fix: in Cursor, run \"Reopen Folder in WSL\" (Ctrl+Shift+P). The window")
	fmt.Fprintln(w, "  title should end in [WSL: "+distro+"]. From a WSL terminal, `cursor .`")
	fmt.Fprintln(w, "  also works once the WSL server is installed.")
	fmt.Fprintln(w, "  Already switched? This warning stops once the leftover Windows-side")
	fmt.Fprintf(w, "  activity is older than %d days.\n", int(cursor.UNCEvidenceWindow.Hours()/24))
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
		otherShadow := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		if otherShadow == shadowBranch && state.StepCount > 0 {
			return false, nil
		}
	}

	return true, nil
}
