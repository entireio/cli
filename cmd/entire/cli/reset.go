package cli

import (
	"errors"
	"fmt"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newResetCmd() *cobra.Command {
	var forceFlag bool

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
  - Delete the shadow branch (entire/<commit-hash>)

Example: If HEAD is at commit abc1234567890, the command will:
  1. Find all .json files in .git/entire-sessions/ with "base_commit": "abc1234567890"
  2. Delete those session files (e.g., 2026-02-02-xyz123.json, 2026-02-02-abc456.json)
  3. Delete the shadow branch entire/abc1234

Without --force, prompts for confirmation before deleting.`,
		RunE: func(_ *cobra.Command, _ []string) error {
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

	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Skip confirmation prompt")

	return cmd
}
