package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage agent sessions tracked by Entire",
	}

	cmd.AddCommand(newStopCmd())

	return cmd
}

func newStopCmd() *cobra.Command {
	var allFlag bool
	var forceFlag bool

	cmd := &cobra.Command{
		Use:   "stop [session-id]",
		Short: "Stop one or more active sessions",
		Long: `Mark one or more active sessions as ended.

Fires EventSessionStop through the state machine with a no-op action handler,
so no condensation or checkpoint-writing occurs. To flush pending work, commit first.

Examples:
  entire sessions stop                     No sessions: exits. One session: confirm and stop. Multiple: show selector
  entire sessions stop <session-id>        Stop a specific session by ID
  entire sessions stop --all               Stop all active sessions in current worktree
  entire sessions stop --force             Skip confirmation prompt`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			var sessionID string
			if len(args) > 0 {
				sessionID = args[0]
			}

			if allFlag && sessionID != "" {
				return errors.New("--all and session ID argument are mutually exclusive")
			}

			// Check if in git repository
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				return errors.New("not a git repository")
			}

			return runStop(ctx, cmd, sessionID, allFlag, forceFlag)
		},
	}

	cmd.Flags().BoolVar(&allFlag, "all", false, "Stop all active sessions in current worktree")
	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Skip confirmation prompt")

	return cmd
}

// runStop is the main logic for the stop command.
func runStop(ctx context.Context, cmd *cobra.Command, sessionID string, all, force bool) error {
	// --session path: stop a specific session by explicit ID (no worktree scoping).
	if sessionID != "" {
		return runStopSession(ctx, cmd, sessionID, force)
	}

	// List all session states
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	activeSessions := filterActiveSessions(states)

	// --all path: stop all active sessions in current worktree (scoped inside runStopAll).
	if all {
		return runStopAll(ctx, cmd, activeSessions, force)
	}

	// No-flags path: scope to current worktree before presenting options.
	// RunE already validated the git repo, so this call succeeds in practice.
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve worktree root: %w", err)
	}
	var scoped []*strategy.SessionState
	for _, s := range activeSessions {
		if s.WorktreePath == worktreePath || s.WorktreePath == "" {
			scoped = append(scoped, s)
		}
	}

	if len(scoped) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No active sessions.")
		return nil
	}

	// One active session: confirm + stop.
	if len(scoped) == 1 {
		return runStopSession(ctx, cmd, scoped[0].SessionID, force)
	}

	// Multiple active sessions: show TUI multi-select.
	return runStopMultiSelect(ctx, cmd, scoped, force)
}

// filterActiveSessions returns sessions in PhaseIdle or PhaseActive — all sessions
// that have not been explicitly ended. Both phases are considered stoppable: IDLE
// means the agent finished its last turn but the session is still open.
//
// The dual check (Phase != PhaseEnded AND EndedAt == nil) is intentionally stricter
// than status.go's EndedAt-only check: it ensures sessions where only the state
// machine transition succeeded (Phase=Ended) but EndedAt was never written are still
// treated as ended, avoiding an accidental re-stop of a partially-ended session.
func filterActiveSessions(states []*strategy.SessionState) []*strategy.SessionState {
	var active []*strategy.SessionState
	for _, s := range states {
		if s == nil {
			continue
		}
		if s.Phase != session.PhaseEnded && s.EndedAt == nil {
			active = append(active, s)
		}
	}
	return active
}

// runStopSession stops a single session by ID, with optional confirmation.
func runStopSession(ctx context.Context, cmd *cobra.Command, sessionID string, force bool) error {
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session: %w", err)
	}
	if state == nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Session not found.")
		return NewSilentError(fmt.Errorf("session not found: %s", sessionID))
	}

	if state.Phase == session.PhaseEnded || state.EndedAt != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Session %s is already stopped.\n", sessionID)
		return nil
	}

	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Stop session %s?", sessionID)).
					Value(&confirmed),
			),
		)
		if err := form.Run(); err != nil {
			return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
		}
		if !confirmed {
			fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
			return nil
		}
	}

	return stopSessionAndPrint(ctx, cmd, state)
}

// runStopAll stops all active sessions scoped to the current worktree.
func runStopAll(ctx context.Context, cmd *cobra.Command, activeSessions []*strategy.SessionState, force bool) error {
	// Scope to current worktree. Sessions with an empty WorktreePath predate
	// worktree-path tracking and cannot be attributed to any specific worktree —
	// including them here prevents them from being permanently unreachable via --all.
	// RunE already validated the git repo, so this call succeeds in practice.
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve worktree root: %w", err)
	}

	var toStop []*strategy.SessionState
	for _, s := range activeSessions {
		if s.WorktreePath == worktreePath || s.WorktreePath == "" {
			toStop = append(toStop, s)
		}
	}

	if len(toStop) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No active sessions.")
		return nil
	}

	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Stop %d session(s)?", len(toStop))).
					Value(&confirmed),
			),
		)
		if err := form.Run(); err != nil {
			return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
		}
		if !confirmed {
			fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
			return nil
		}
	}

	return stopSelectedSessions(ctx, cmd, toStop)
}

// runStopMultiSelect shows a TUI multi-select for multiple active sessions.
func runStopMultiSelect(ctx context.Context, cmd *cobra.Command, activeSessions []*strategy.SessionState, force bool) error {
	options := make([]huh.Option[string], len(activeSessions))
	for i, s := range activeSessions {
		label := fmt.Sprintf("%s · %s", s.AgentType, s.SessionID)
		if s.LastPrompt != "" {
			label = fmt.Sprintf("%s · %q", label, s.LastPrompt)
		}
		options[i] = huh.NewOption(label, s.SessionID)
	}

	var selectedIDs []string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select sessions to stop").
				Description("Use space to select, enter to confirm.").
				Options(options...).
				Value(&selectedIDs),
		),
	)
	if err := form.Run(); err != nil {
		return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
	}

	if len(selectedIDs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
		return nil
	}

	// Build a map for quick lookup
	stateByID := make(map[string]*strategy.SessionState, len(activeSessions))
	for _, s := range activeSessions {
		stateByID[s.SessionID] = s
	}

	// Confirm only if not forcing
	if !force {
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Stop %d session(s)?", len(selectedIDs))).
					Value(&confirmed),
			),
		)
		if err := form.Run(); err != nil {
			return handleFormCancellation(cmd.OutOrStdout(), "Stop", err)
		}
		if !confirmed {
			fmt.Fprintln(cmd.OutOrStdout(), "Stop cancelled.")
			return nil
		}
	}

	var toStop []*strategy.SessionState
	for _, id := range selectedIDs {
		if s, ok := stateByID[id]; ok {
			toStop = append(toStop, s)
		} else {
			// Session was concurrently stopped between form render and confirmation.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: session %s no longer found, skipping.\n", id)
		}
	}
	if len(toStop) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions to stop.")
		return nil
	}
	return stopSelectedSessions(ctx, cmd, toStop)
}

// stopSelectedSessions stops each session in the list and prints a result line.
// Errors from individual sessions are accumulated so a single failure does not
// prevent remaining sessions from being stopped. Each failure is printed to stderr
// immediately so the user knows which sessions could not be stopped.
func stopSelectedSessions(ctx context.Context, cmd *cobra.Command, sessions []*strategy.SessionState) error {
	var errs []error
	for _, s := range sessions {
		if err := stopSessionAndPrint(ctx, cmd, s); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "✗ %v\n", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopSessionAndPrint stops a session and prints a summary line.
// Fields needed for output are read before calling markSessionEnded because
// markSessionEnded loads and operates on its own copy of the session state by ID —
// it does not update the caller's state pointer.
func stopSessionAndPrint(ctx context.Context, cmd *cobra.Command, state *strategy.SessionState) error {
	sessionID := state.SessionID
	lastCheckpointID := state.LastCheckpointID
	stepCount := state.StepCount

	if err := markSessionEnded(ctx, nil, sessionID); err != nil {
		return fmt.Errorf("failed to stop session %s: %w", sessionID, err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Session %s stopped.\n", sessionID)
	switch {
	case lastCheckpointID != "":
		fmt.Fprintf(cmd.OutOrStdout(), "  Checkpoint: %s\n", lastCheckpointID)
	case stepCount > 0:
		fmt.Fprintln(cmd.OutOrStdout(), "  Work will be captured in your next checkpoint.")
	default:
		fmt.Fprintln(cmd.OutOrStdout(), "  No work recorded.")
	}
	return nil
}
