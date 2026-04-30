package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
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

  When checkpoints_v2 is enabled:
  2. Disconnected v2 /main ref: same detection for v2 refs under refs/entire/.
  3. v2 ref existence: verifies /main and /full/current refs exist consistently.
  4. v2 checkpoint counts: verifies /main and /full/current checkpoint counts are consistent.
  5. v2 generation health: checks archived generations for valid metadata.

  6. Stuck sessions: sessions stuck in ACTIVE or ENDED phase that need cleanup.

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

	// v2 checks (only when checkpoints_v2 is enabled)
	ctx := cmd.Context()
	if settings.IsCheckpointsV2Enabled(ctx) {
		// Check 2: Disconnected v2 /main ref
		v2DisconnectedErr := checkDisconnectedV2Main(cmd, force)
		if v2DisconnectedErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: v2 /main check failed: %v\n", v2DisconnectedErr)
			if finalErr == nil {
				finalErr = NewSilentError(fmt.Errorf("v2 /main check failed: %w", v2DisconnectedErr))
			}
		}

		repo, repoErr := openRepository(ctx)
		if repoErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: could not open repository for v2 checks: %v\n", repoErr)
			if finalErr == nil {
				finalErr = NewSilentError(fmt.Errorf("v2 checks failed: %w", repoErr))
			}
		} else {
			// Check 3: v2 ref existence
			if refErr := checkV2RefExistence(cmd, repo); refErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Error: v2 ref existence check failed: %v\n", refErr)
				if finalErr == nil {
					finalErr = NewSilentError(fmt.Errorf("v2 ref check failed: %w", refErr))
				}
			}

			// Check 4: v2 checkpoint count consistency
			if countErr := checkV2CheckpointCounts(cmd, repo); countErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Error: v2 checkpoint count check failed: %v\n", countErr)
				if finalErr == nil {
					finalErr = NewSilentError(fmt.Errorf("v2 count check failed: %w", countErr))
				}
			}

			// Check 5: v2 generation health
			if genErr := checkV2GenerationHealth(cmd, repo); genErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Error: v2 generation health check failed: %v\n", genErr)
				if finalErr == nil {
					finalErr = NewSilentError(fmt.Errorf("v2 generation check failed: %w", genErr))
				}
			}
		}

		fmt.Fprintln(cmd.OutOrStdout())
	}

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
		if !state.IsStuckActive() {
			return nil
		}

		var reason string
		if state.LastInteractionTime != nil {
			reason = fmt.Sprintf("active, last interaction %s ago", now.Sub(*state.LastInteractionTime).Truncate(time.Minute))
		} else {
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

	ctx := cmd.Context()
	remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	disconnected, err := strategy.IsMetadataDisconnected(ctx, repo, remoteRefName)
	if err != nil {
		return fmt.Errorf("could not check metadata branch state: %w", err)
	}

	w := cmd.OutOrStdout()

	if !disconnected {
		fmt.Fprintln(w, "✓ Metadata branches: OK")
		return nil
	}

	fmt.Fprintln(w, "Metadata branches: DISCONNECTED")
	fmt.Fprintln(w, "  Local and remote entire/checkpoints/v1 branches share no common ancestor.")
	fmt.Fprintln(w, "  Some remote checkpoints may not be visible locally.")
	fmt.Fprintln(w, "  Fix: cherry-pick local checkpoints onto remote tip (preserves all data).")

	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Fix disconnected metadata branches?").
					Value(&confirmed),
			),
		)
		if formErr := form.Run(); formErr != nil {
			if errors.Is(formErr, huh.ErrUserAborted) {
				return nil
			}
			return fmt.Errorf("prompt failed: %w", formErr)
		}
		if !confirmed {
			fmt.Fprintln(w, "  -> Skipped")
			return nil
		}
	}

	if fixErr := strategy.ReconcileDisconnectedMetadataBranch(ctx, repo, remoteRefName, cmd.ErrOrStderr()); fixErr != nil {
		return fmt.Errorf("failed to reconcile metadata branches: %w", fixErr)
	}

	fmt.Fprintln(w, "  ✓ Fixed: metadata branches reconciled")
	return nil
}

// checkDisconnectedV2Main detects and optionally repairs disconnected
// local/remote v2 /main refs.
func checkDisconnectedV2Main(cmd *cobra.Command, force bool) error {
	repo, err := openRepository(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	ctx := cmd.Context()
	configured := remote.Configured(ctx)
	remoteName := migrateRemoteName
	if configured {
		resolvedRemote, resolveErr := remote.FetchURL(ctx)
		if resolveErr != nil {
			return fmt.Errorf("checkpoint_remote is configured but could not be resolved: %w", resolveErr)
		}
		remoteName = resolvedRemote
	}

	disconnected, err := strategy.IsV2MainDisconnected(ctx, repo, remoteName)
	if err != nil {
		// If no checkpoint_remote is configured and origin doesn't exist or is
		// unreachable, treat as "can't check" rather than a hard failure — mirrors
		// the v1 behavior which no-ops when the remote-tracking ref is absent.
		if !configured {
			fmt.Fprintln(cmd.OutOrStdout(), "✓ v2 /main ref: OK (no remote to compare)")
			return nil
		}
		return fmt.Errorf("could not check v2 /main ref state: %w", err)
	}

	w := cmd.OutOrStdout()

	if !disconnected {
		fmt.Fprintln(w, "✓ v2 /main ref: OK")
		return nil
	}

	fmt.Fprintln(w, "v2 /main ref: DISCONNECTED")
	fmt.Fprintln(w, "  Local and remote v2 /main refs share no common ancestor.")
	fmt.Fprintln(w, "  Fix: cherry-pick local checkpoints onto remote tip (preserves all data).")

	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Fix disconnected v2 /main ref?").
					Value(&confirmed),
			),
		)
		if formErr := form.Run(); formErr != nil {
			if errors.Is(formErr, huh.ErrUserAborted) {
				return nil
			}
			return fmt.Errorf("prompt failed: %w", formErr)
		}
		if !confirmed {
			fmt.Fprintln(w, "  -> Skipped")
			return nil
		}
	}

	if fixErr := strategy.ReconcileDisconnectedV2Ref(ctx, repo, remoteName, cmd.ErrOrStderr()); fixErr != nil {
		return fmt.Errorf("failed to reconcile v2 /main ref: %w", fixErr)
	}

	fmt.Fprintln(w, "  ✓ Fixed: v2 /main ref reconciled")
	return nil
}

// checkV2GenerationHealth verifies that archived /full/* generations are well-formed.
// Checks: generation.json exists and is valid, timestamps are sane, generation has checkpoints,
// and generation sequence numbers are contiguous.
func checkV2GenerationHealth(cmd *cobra.Command, repo *git.Repository) error {
	w := cmd.OutOrStdout()

	v2Store := checkpoint.NewV2GitStore(repo, "origin")

	archived, err := v2Store.ListArchivedGenerations()
	if err != nil {
		return fmt.Errorf("failed to list archived generations: %w", err)
	}

	if len(archived) == 0 {
		fmt.Fprintln(w, "✓ v2 generations: OK (no archived generations)")
		return nil
	}

	var warnings []string

	for _, genName := range archived {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + genName)

		_, treeHash, refErr := v2Store.GetRefState(refName)
		if refErr != nil {
			warnings = append(warnings, fmt.Sprintf("generation %s: cannot read ref: %v", genName, refErr))
			continue
		}

		gen, genErr := v2Store.ReadGeneration(treeHash)
		if genErr != nil {
			warnings = append(warnings, fmt.Sprintf("generation %s: failed to read generation.json: %v", genName, genErr))
			continue
		}

		hasOldest := !gen.OldestCheckpointAt.IsZero()
		hasNewest := !gen.NewestCheckpointAt.IsZero()

		switch {
		case !hasOldest && !hasNewest:
			// ReadGeneration returns zero-value when the file is absent
			warnings = append(warnings, fmt.Sprintf("generation %s: WARNING — missing generation.json", genName))
		case hasOldest != hasNewest:
			warnings = append(warnings, fmt.Sprintf("generation %s: WARNING — incomplete generation.json (partial timestamps)", genName))
		case gen.OldestCheckpointAt.After(gen.NewestCheckpointAt):
			warnings = append(warnings, fmt.Sprintf("generation %s: WARNING — invalid timestamps (oldest > newest)", genName))
		}

		cpCount, countErr := v2Store.CountCheckpointsInTree(treeHash)
		if countErr != nil {
			warnings = append(warnings, fmt.Sprintf("generation %s: failed to count checkpoints: %v", genName, countErr))
			continue
		}
		if cpCount == 0 {
			warnings = append(warnings, fmt.Sprintf("generation %s: WARNING — empty (no checkpoint shards)", genName))
		}
	}

	if len(archived) > 1 {
		for i := 1; i < len(archived); i++ {
			prev, prevErr := strconv.ParseInt(archived[i-1], 10, 64)
			curr, currErr := strconv.ParseInt(archived[i], 10, 64)
			if prevErr != nil || currErr != nil {
				continue
			}
			if curr-prev > 1 {
				first := prev + 1
				last := curr - 1
				if first == last {
					warnings = append(warnings, fmt.Sprintf("INFO — gap in generation sequence (%013d missing)", first))
				} else {
					warnings = append(warnings, fmt.Sprintf("INFO — gap in generation sequence (%013d–%013d missing)", first, last))
				}
			}
		}
	}

	if len(warnings) > 0 {
		fmt.Fprintf(w, "v2 generations: %d issue(s) found in %d archived generation(s):\n", len(warnings), len(archived))
		for _, warning := range warnings {
			fmt.Fprintf(w, "  %s\n", warning)
		}
		return fmt.Errorf("v2 generation health: %d issue(s) found", len(warnings))
	}

	fmt.Fprintf(w, "✓ v2 generations: OK (%d archived)\n", len(archived))
	return nil
}

// checkV2CheckpointCounts verifies checkpoint count consistency between /main and /full/current.
// /main is permanent (accumulates all checkpoints), /full/current holds only the current generation.
// So main count >= full/current count. If full/current exceeds main, a dual-write partially failed.
// Skips silently if either ref doesn't exist (already covered by checkV2RefExistence).
func checkV2CheckpointCounts(cmd *cobra.Command, repo *git.Repository) error {
	w := cmd.OutOrStdout()

	v2Store := checkpoint.NewV2GitStore(repo, "origin")

	mainRefName := plumbing.ReferenceName(paths.V2MainRefName)
	fullRefName := plumbing.ReferenceName(paths.V2FullCurrentRefName)

	_, mainTreeHash, mainErr := v2Store.GetRefState(mainRefName)
	_, fullTreeHash, fullErr := v2Store.GetRefState(fullRefName)

	// Skip only when ref is missing (already covered by checkV2RefExistence).
	if mainErr != nil {
		if errors.Is(mainErr, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return fmt.Errorf("failed to read /main ref: %w", mainErr)
	}
	if fullErr != nil {
		if errors.Is(fullErr, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return fmt.Errorf("failed to read /full/current ref: %w", fullErr)
	}

	mainCount, err := v2Store.CountCheckpointsInTree(mainTreeHash)
	if err != nil {
		return fmt.Errorf("failed to count /main checkpoints: %w", err)
	}

	fullCount, err := v2Store.CountCheckpointsInTree(fullTreeHash)
	if err != nil {
		return fmt.Errorf("failed to count /full/current checkpoints: %w", err)
	}

	if fullCount > mainCount {
		fmt.Fprintf(w, "v2 checkpoint counts: INCONSISTENT — /full/current has %d checkpoints but /main has only %d\n", fullCount, mainCount)
		return fmt.Errorf("v2 checkpoint counts inconsistent: /full/current (%d) exceeds /main (%d)", fullCount, mainCount)
	}

	fmt.Fprintf(w, "✓ v2 checkpoint counts: OK (main: %d, full/current: %d)\n", mainCount, fullCount)
	return nil
}

// checkV2RefExistence verifies that v2 refs exist (or both are absent for a fresh repo).
// One ref without the other suggests a partial initialization.
func checkV2RefExistence(cmd *cobra.Command, repo *git.Repository) error {
	w := cmd.OutOrStdout()

	mainRefName := plumbing.ReferenceName(paths.V2MainRefName)
	fullRefName := plumbing.ReferenceName(paths.V2FullCurrentRefName)

	_, mainErr := repo.Reference(mainRefName, true)
	_, fullErr := repo.Reference(fullRefName, true)
	if mainErr != nil && !errors.Is(mainErr, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("failed to read /main ref: %w", mainErr)
	}
	if fullErr != nil && !errors.Is(fullErr, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("failed to read /full/current ref: %w", fullErr)
	}

	hasMain := mainErr == nil
	hasFull := fullErr == nil

	switch {
	case hasMain && hasFull:
		fmt.Fprintln(w, "✓ v2 refs: OK")
	case !hasMain && !hasFull:
		fmt.Fprintln(w, "✓ v2 refs: OK (no checkpoints written yet)")
	case hasMain && !hasFull:
		fmt.Fprintln(w, "v2 refs: INCONSISTENT — /main exists but /full/current is missing")
		return errors.New("v2 refs inconsistent: /main exists but /full/current is missing")
	case !hasMain && hasFull:
		fmt.Fprintln(w, "v2 refs: INCONSISTENT — /full/current exists but /main is missing")
		return errors.New("v2 refs inconsistent: /full/current exists but /main is missing")
	}

	return nil
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
