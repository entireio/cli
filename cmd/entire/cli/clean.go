package cli

import (
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newCleanCmd() *cobra.Command {
	var forceFlag bool

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean up orphaned session data",
		Long: `Remove orphaned session data that wasn't cleaned up automatically.

This command finds and removes orphaned data from any strategy:

  Shadow branches (entire/<commit-hash>)
    Created by manual-commit strategy. Normally auto-cleaned when sessions
    are condensed during commits.

  Session state files (.git/entire-sessions/)
    Track active sessions. Orphaned when no checkpoints or shadow branches
    reference them.

  Checkpoint metadata (entire/checkpoints/v1 branch)
    For auto-commit checkpoints: orphaned when commits are rebased/squashed
    and no commit references the checkpoint ID anymore.
    Manual-commit checkpoints are permanent (condensed history) and are
    never considered orphaned.

Default: shows a preview of items that would be deleted.
With --force, actually deletes the orphaned items.

The entire/checkpoints/v1 branch itself is never deleted.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClean(cmd.OutOrStdout(), forceFlag)
		},
	}

	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Actually delete items (default: dry run)")

	return cmd
}

func runClean(w io.Writer, force bool) error {
	// List all cleanup items
	items, err := strategy.ListAllCleanupItems()
	if err != nil {
		return fmt.Errorf("failed to list orphaned items: %w", err)
	}

	return runCleanWithItems(w, force, items)
}

// runCleanWithItems is the core logic for cleaning orphaned items.
// Separated for testability.
func runCleanWithItems(w io.Writer, force bool, items []strategy.CleanupItem) error {
	// Handle no items case
	if len(items) == 0 {
		fmt.Fprintln(w, "No orphaned items to clean up.")
		return nil
	}

	// Group items by type for display
	var branches, states, checkpoints []strategy.CleanupItem
	for _, item := range items {
		switch item.Type {
		case strategy.CleanupTypeShadowBranch:
			branches = append(branches, item)
		case strategy.CleanupTypeSessionState:
			states = append(states, item)
		case strategy.CleanupTypeCheckpoint:
			checkpoints = append(checkpoints, item)
		}
	}

	// Preview mode (default)
	if !force {
		fmt.Fprintf(w, "Found %d orphaned items:\n\n", len(items))

		if len(branches) > 0 {
			fmt.Fprintf(w, "Shadow branches (%d):\n", len(branches))
			for _, item := range branches {
				fmt.Fprintf(w, "  %s\n", item.ID)
			}
			fmt.Fprintln(w)
		}

		if len(states) > 0 {
			fmt.Fprintf(w, "Session states (%d):\n", len(states))
			for _, item := range states {
				fmt.Fprintf(w, "  %s\n", item.ID)
			}
			fmt.Fprintln(w)
		}

		if len(checkpoints) > 0 {
			fmt.Fprintf(w, "Checkpoint metadata (%d):\n", len(checkpoints))
			for _, item := range checkpoints {
				fmt.Fprintf(w, "  %s\n", item.ID)
			}
			fmt.Fprintln(w)
		}

		fmt.Fprintln(w, "Run with --force to delete these items.")
		return nil
	}

	// Force mode - delete items
	result, err := strategy.DeleteAllCleanupItems(items)
	if err != nil {
		return fmt.Errorf("failed to delete orphaned items: %w", err)
	}

	// Report results
	totalDeleted := len(result.ShadowBranches) + len(result.SessionStates) + len(result.Checkpoints)
	totalFailed := len(result.FailedBranches) + len(result.FailedStates) + len(result.FailedCheckpoints)

	if totalDeleted > 0 {
		fmt.Fprintf(w, "Deleted %d items:\n", totalDeleted)

		if len(result.ShadowBranches) > 0 {
			fmt.Fprintf(w, "\n  Shadow branches (%d):\n", len(result.ShadowBranches))
			for _, branch := range result.ShadowBranches {
				fmt.Fprintf(w, "    %s\n", branch)
			}
		}

		if len(result.SessionStates) > 0 {
			fmt.Fprintf(w, "\n  Session states (%d):\n", len(result.SessionStates))
			for _, state := range result.SessionStates {
				fmt.Fprintf(w, "    %s\n", state)
			}
		}

		if len(result.Checkpoints) > 0 {
			fmt.Fprintf(w, "\n  Checkpoints (%d):\n", len(result.Checkpoints))
			for _, cp := range result.Checkpoints {
				fmt.Fprintf(w, "    %s\n", cp)
			}
		}
	}

	if totalFailed > 0 {
		fmt.Fprintf(w, "\nFailed to delete %d items:\n", totalFailed)

		if len(result.FailedBranches) > 0 {
			fmt.Fprintf(w, "\n  Shadow branches:\n")
			for _, branch := range result.FailedBranches {
				fmt.Fprintf(w, "    %s\n", branch)
			}
		}

		if len(result.FailedStates) > 0 {
			fmt.Fprintf(w, "\n  Session states:\n")
			for _, state := range result.FailedStates {
				fmt.Fprintf(w, "    %s\n", state)
			}
		}

		if len(result.FailedCheckpoints) > 0 {
			fmt.Fprintf(w, "\n  Checkpoints:\n")
			for _, cp := range result.FailedCheckpoints {
				fmt.Fprintf(w, "    %s\n", cp)
			}
		}

		return fmt.Errorf("failed to delete %d items", totalFailed)
	}

	return nil
}
