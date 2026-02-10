package cli

import (
	"errors"
	"fmt"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newResetCmd() *cobra.Command {
	var forceFlag bool
	var sessionFlag string

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset the shadow branch and session state for current HEAD",
		Long: `Reset deletes the shadow branch and session state for the current HEAD commit.

This allows starting fresh without existing checkpoints on your current commit.

Only works with the manual-commit strategy. For auto-commit strategy,
use Git directly: git reset --hard <commit>

The command will:
  - Find all sessions where base_commit matches the current HEAD
  - Delete each session state file (.git/entire-sessions/<session-id>.json)
  - Delete the shadow branch (entire/<commit-hash>-<worktree-hash>)

Use --session <id> to reset a single session instead of all sessions.

Example: If HEAD is at commit abc1234567890, the command will:
  1. Find all .json files in .git/entire-sessions/ with "base_commit": "abc1234567890"
  2. Delete those session files (e.g., 2026-02-02-xyz123.json, 2026-02-02-abc456.json)
  3. Delete the shadow branch entire/abc1234-fd5432

Without --force, prompts for confirmation before deleting.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if in git repository
			if _, err := paths.RepoRoot(); err != nil {
				return errors.New("not a git repository")
			}

			// Get current strategy
			strat := GetStrategy()

			// Check if strategy supports reset
			resetter, ok := strat.(strategy.SessionResetter)
			if !ok {
				return fmt.Errorf("strategy %s does not support reset", strat.Name())
			}

			// Handle --session flag: reset a single session
			if sessionFlag != "" {
				return runResetSession(cmd, resetter, sessionFlag, forceFlag)
			}

			// Check for active sessions before bulk reset
			if !forceFlag {
				activeSessions, err := activeSessionsOnCurrentHead()
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not check for active sessions: %v\n", err)
					fmt.Fprintln(cmd.ErrOrStderr(), "Use --force to override.")
					return nil
				}
				if len(activeSessions) > 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "Active sessions detected on current HEAD:")
					for _, s := range activeSessions {
						fmt.Fprintf(cmd.ErrOrStderr(), "  %s (phase: %s)\n", s.SessionID, s.Phase)
					}
					fmt.Fprintln(cmd.ErrOrStderr(), "Use --force to override or wait for sessions to finish.")
					return nil
				}
			}

			if !forceFlag {
				var confirmed bool

				form := NewAccessibleForm(
					huh.NewGroup(
						huh.NewConfirm().
							Title("Reset session data?").
							Value(&confirmed),
					),
				)

				if err := form.Run(); err != nil {
					if errors.Is(err, huh.ErrUserAborted) {
						return nil
					}
					return fmt.Errorf("failed to get confirmation: %w", err)
				}

				if !confirmed {
					return nil
				}
			}

			// Call strategy's Reset method
			if err := resetter.Reset(); err != nil {
				return fmt.Errorf("reset failed: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Skip confirmation prompt and override active session guard")
	cmd.Flags().StringVar(&sessionFlag, "session", "", "Reset a specific session by ID")

	return cmd
}

// runResetSession handles the --session flag: reset a single session.
func runResetSession(cmd *cobra.Command, resetter strategy.SessionResetter, sessionID string, force bool) error {
	// Verify the session exists
	state, err := strategy.LoadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if !force {
		var confirmed bool

		title := fmt.Sprintf("Reset session %s?", sessionID)
		description := fmt.Sprintf("Phase: %s, Checkpoints: %d", state.Phase, state.StepCount)

		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(title).
					Description(description).
					Value(&confirmed),
			),
		)

		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return fmt.Errorf("failed to get confirmation: %w", err)
		}

		if !confirmed {
			return nil
		}
	}

	if err := resetter.ResetSession(sessionID); err != nil {
		return fmt.Errorf("reset session failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Session %s has been reset. File changes remain in the working directory.\n", sessionID)
	return nil
}

// activeSessionsOnCurrentHead returns sessions on the current HEAD
// that are in an active phase (ACTIVE or ACTIVE_COMMITTED).
func activeSessionsOnCurrentHead() ([]*session.State, error) {
	repo, err := openRepository()
	if err != nil {
		return nil, err
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}
	currentHead := head.Hash().String()

	states, err := strategy.ListSessionStates()
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}

	var active []*session.State
	for _, state := range states {
		if state.BaseCommit != currentHead {
			continue
		}
		if state.Phase.IsActive() {
			active = append(active, state)
		}
	}

	return active, nil
}
