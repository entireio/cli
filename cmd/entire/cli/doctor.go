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

	switch {
	case state.Phase.IsActive():
		var reason string
		switch {
		case state.OwnerExited():
			// Detected immediately (no timeout wait): the owning agent process
			// is gone. Normally finalized up front in runSessionsFix; this
			// branch covers a session that couldn't be finalized there.
			pid := 0
			if state.Owner != nil {
				pid = state.Owner.PID
			}
			reason = fmt.Sprintf("agent process %d exited (no longer running)", pid)
		case !state.IsStuckActive():
			return nil
		case state.LastInteractionTime != nil:
			reason = fmt.Sprintf("active, last interaction %s ago", now.Sub(*state.LastInteractionTime).Truncate(time.Minute))
		default:
			reason = fmt.Sprintf("active, started %s ago with no recorded interaction", now.Sub(state.StartedAt).Truncate(time.Minute))
		}

		return &stuckSession{
			State:             state,
			Reason:            reason,
			ShadowBranch:      shadowBranch,
			HasShadowBranch:   hasShadowBranch,
			CheckpointCount:   state.StepCount,
			FilesTouchedCount: len(state.FilesTouched),
		}

	case state.Phase == session.PhaseEnded:
		// Ended sessions are stuck if they have uncondensed data
		if state.StepCount <= 0 || !hasShadowBranch {
			return nil
		}

		return &stuckSession{
			State:             state,
			Reason:            "ended with uncondensed checkpoint data",
			ShadowBranch:      shadowBranch,
			HasShadowBranch:   hasShadowBranch,
			CheckpointCount:   state.StepCount,
			FilesTouchedCount: len(state.FilesTouched),
		}

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
	if !refs.PrimaryFetchableFromOrigin() {
		fmt.Fprintf(w, "✓ Metadata branches: OK (primary ref %s is not pushed to origin)\n", refs.Primary)
		return nil
	}
	remoteRefName := plumbing.NewRemoteReferenceName("origin", refs.Primary.Short())
	disconnected, err := strategy.IsMetadataDisconnected(ctx, repo, remoteRefName)
	if err != nil {
		return fmt.Errorf("could not check metadata branch state: %w", err)
	}

	if !disconnected {
		fmt.Fprintln(w, "✓ Metadata branches: OK")
		return nil
	}

	fmt.Fprintln(w, "Metadata branches: DISCONNECTED")
	fmt.Fprintf(w, "  Local and remote %s branches share no common ancestor.\n", refs.Primary.Short())
	fmt.Fprintln(w, "  Some remote checkpoints may not be visible locally.")
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
		fmt.Fprintln(w, "Global tracking: UNUSABLE EXCLUDE PATTERNS")
		fmt.Fprintln(w, "  Fail closed: each of these deactivates global tracking in every repo it is checked against.")
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

	// Untrusted enrolled repo: informational, never a failure — holding
	// checkpoint sync is the intended state until the user opts in. RED here
	// would train users to treat the consent gate as breakage.
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
		otherShadow := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		if otherShadow == shadowBranch && state.StepCount > 0 {
			return false, nil
		}
	}

	return true, nil
}
