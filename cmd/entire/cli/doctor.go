package cli

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/spf13/cobra"
)

// stalenessThreshold is the duration after which an active session is considered stuck.
const stalenessThreshold = 1 * time.Hour

func newDoctorCmd() *cobra.Command {
	var forceFlag bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Fix stuck sessions",
		Long: `Scan for stuck or problematic sessions and offer to fix them.

A session is considered stuck if:
  - It is in ACTIVE phase with no interaction for over 1 hour
  - It is in ENDED phase with uncondensed checkpoint data on a shadow branch

For each stuck session, you can choose to:
  - Condense: Save session data to permanent storage (entire/checkpoints/v1 branch)
  - Discard: Remove the session state and shadow branch data
  - Skip: Leave the session as-is

Use --force to condense all fixable sessions without prompting.  Sessions that can't
be condensed will be discarded.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionsFix(cmd, forceFlag)
		},
	}

	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Fix all stuck sessions without prompting (condense if possible, otherwise discard)")

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
	// Load all session states
	states, err := strategy.ListSessionStates()
	if err != nil {
		return fmt.Errorf("failed to list session states: %w", err)
	}

	if len(states) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No stuck sessions found.")
		return nil
	}

	// Open repository to check shadow branches (uses worktree-aware helper)
	repo, err := openRepository()
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
		return nil
	}

	// Get the current strategy for condense operations
	strat := GetStrategy()
	condenser, canCondense := strat.(strategy.SessionCondenser)

	fmt.Fprintf(cmd.OutOrStdout(), "Found %d stuck session(s):\n\n", len(stuck))

	for _, ss := range stuck {
		displayStuckSession(cmd, ss)

		if force {
			if canCondense && ss.HasShadowBranch && ss.CheckpointCount > 0 {
				if err := condenser.CondenseSessionByID(ss.State.SessionID); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to condense session %s: %v\n", ss.State.SessionID, err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  -> Condensed session %s\n\n", ss.State.SessionID)
				}
			} else {
				// Discard if we can't condense
				if err := discardSession(ss, repo, cmd.ErrOrStderr()); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to discard session %s: %v\n", ss.State.SessionID, err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  -> Discarded session %s\n\n", ss.State.SessionID)
				}
			}
			continue
		}

		// Interactive: prompt for action
		action, err := promptSessionAction(ss, canCondense)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return fmt.Errorf("failed to get action: %w", err)
		}

		switch action {
		case "condense":
			if !canCondense {
				fmt.Fprintf(cmd.ErrOrStderr(), "Strategy %s does not support condensation\n", strat.Name())
				continue
			}
			if err := condenser.CondenseSessionByID(ss.State.SessionID); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to condense session %s: %v\n", ss.State.SessionID, err)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  -> Condensed session %s\n\n", ss.State.SessionID)
			}
		case "discard":
			if err := discardSession(ss, repo, cmd.ErrOrStderr()); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to discard session %s: %v\n", ss.State.SessionID, err)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  -> Discarded session %s\n\n", ss.State.SessionID)
			}
		case "skip":
			fmt.Fprintf(cmd.OutOrStdout(), "  -> Skipped\n\n")
		}
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
		// Active sessions are stuck if no interaction for over the staleness threshold
		isStale := state.LastInteractionTime == nil || now.Sub(*state.LastInteractionTime) > stalenessThreshold
		if !isStale {
			return nil
		}

		var reason string
		if state.LastInteractionTime != nil {
			reason = fmt.Sprintf("active, last interaction %s ago", now.Sub(*state.LastInteractionTime).Truncate(time.Minute))
		} else {
			reason = "active, no recorded interaction time"
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
func promptSessionAction(ss stuckSession, canCondense bool) (string, error) {
	var action string

	options := make([]huh.Option[string], 0, 3)
	if canCondense && ss.HasShadowBranch && ss.CheckpointCount > 0 {
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
func discardSession(ss stuckSession, _ *git.Repository, errW io.Writer) error {
	// Clear session state file
	if err := strategy.ClearSessionState(ss.State.SessionID); err != nil {
		return fmt.Errorf("failed to clear session state: %w", err)
	}

	// Delete shadow branch if it exists and no other sessions need it
	if ss.HasShadowBranch {
		if shouldDelete, err := canDeleteShadowBranch(ss.ShadowBranch, ss.State.SessionID); err != nil {
			fmt.Fprintf(errW, "Warning: could not check other sessions for shadow branch: %v\n", err)
		} else if shouldDelete {
			if err := strategy.DeleteBranchCLI(ss.ShadowBranch); err != nil {
				// Branch already gone is not an error — keeps discard idempotent
				if !errors.Is(err, strategy.ErrBranchNotFound) {
					return fmt.Errorf("failed to delete shadow branch: %w", err)
				}
			}
		}
	}

	return nil
}

// canDeleteShadowBranch checks if a shadow branch can be safely deleted.
// Returns true if no other sessions (besides excludeSessionID) need this branch.
func canDeleteShadowBranch(shadowBranch, excludeSessionID string) (bool, error) {
	states, err := strategy.ListSessionStates()
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
