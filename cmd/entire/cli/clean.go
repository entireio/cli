package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

func newCleanCmd() *cobra.Command {
	var forceFlag bool
	var allFlag bool
	var dryRunFlag bool
	var sessionFlag string

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean up session data and orphaned Entire data",
		Long: `Clean up Entire session data for the current HEAD commit.

By default, cleans session state and shadow branches for the current HEAD:
  - Session state files (.git/entire-sessions/<session-id>.json)
  - Shadow branch (entire/<commit-hash>-<worktree-hash>)

Use --all to clean all orphaned Entire data across the repository:
  - Orphaned shadow branches
  - Orphaned session state files
  - Orphaned checkpoint entries on entire/checkpoints/v1
  - Temporary files (.entire/tmp/)
The entire/checkpoints/v1 branch itself is preserved.

Use --session <id> to clean a specific session only.

Without --force, prompts for confirmation before deleting.
Use --dry-run to preview what would be deleted without prompting.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			// Validate mutually exclusive flags
			if allFlag && sessionFlag != "" {
				return errors.New("--all and --session cannot be used together")
			}

			// Check if in git repository before initializing logging,
			// to avoid creating .entire/logs in arbitrary directories.
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				return errors.New("not a git repository")
			}

			// Initialize logging
			logging.SetLogLevelGetter(GetLogLevel)
			if err := logging.Init(ctx, ""); err == nil {
				defer logging.Close()
			}

			if allFlag {
				return runCleanAll(ctx, cmd, forceFlag, dryRunFlag)
			}

			if sessionFlag != "" {
				strat := GetStrategy(ctx)
				return runCleanSession(ctx, cmd, strat, sessionFlag, forceFlag, dryRunFlag, "Clean", "cleaned")
			}

			return runCleanCurrentHead(ctx, cmd, forceFlag, dryRunFlag)
		},
	}

	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Skip confirmation prompt and override active session guard")
	cmd.Flags().BoolVarP(&allFlag, "all", "a", false, "Clean all orphaned data across the repository")
	cmd.Flags().BoolVarP(&dryRunFlag, "dry-run", "d", false, "Preview what would be deleted without deleting")
	cmd.Flags().StringVar(&sessionFlag, "session", "", "Clean a specific session by ID")

	return cmd
}

// runCleanCurrentHead cleans session data for the current HEAD commit.
func runCleanCurrentHead(ctx context.Context, cmd *cobra.Command, force, dryRun bool) error {
	strat := GetStrategy(ctx)
	w := cmd.OutOrStdout()

	// Dry-run: show what would be cleaned
	if dryRun {
		return previewCurrentHead(ctx, w)
	}

	// Check for active sessions before cleaning
	if !force {
		activeSessions, err := activeSessionsOnCurrentHead(ctx)
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

	// Prompt for confirmation
	if !force {
		var confirmed bool

		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Clean session data for current HEAD?").
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

	if err := strat.Reset(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return fmt.Errorf("clean failed: %w", err)
	}

	return nil
}

// previewCurrentHead shows what would be cleaned for the current HEAD.
func previewCurrentHead(ctx context.Context, w io.Writer) error {
	repo, err := openRepository(ctx)
	if err != nil {
		return err
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("failed to get worktree path: %w", err)
	}
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		return fmt.Errorf("failed to get worktree ID: %w", err)
	}

	shadowBranchName := checkpoint.ShadowBranchNameForCommit(head.Hash().String(), worktreeID)

	// Check if shadow branch exists
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	_, refErr := repo.Reference(refName, true)
	hasShadowBranch := refErr == nil

	// Find sessions for this commit
	strat := GetStrategy(ctx)
	sessions, err := strat.FindSessionsForCommit(ctx, head.Hash().String())
	if err != nil {
		sessions = nil
	}

	if !hasShadowBranch && len(sessions) == 0 {
		fmt.Fprintln(w, "Nothing to clean for current HEAD.")
		return nil
	}

	fmt.Fprint(w, "Would clean the following items:\n\n")

	if len(sessions) > 0 {
		fmt.Fprintf(w, "Session states (%d):\n", len(sessions))
		for _, s := range sessions {
			fmt.Fprintf(w, "  %s (checkpoints: %d)\n", s.SessionID, s.StepCount)
		}
		fmt.Fprintln(w)
	}

	if hasShadowBranch {
		fmt.Fprintf(w, "Shadow branch:\n  %s\n\n", shadowBranchName)
	}

	fmt.Fprintln(w, "Run without --dry-run to clean these items.")
	return nil
}

// runCleanSession handles the --session flag: clean/reset a single session.
// actionVerb is the capitalized verb (e.g., "Clean" or "Reset") and pastVerb
// is the past tense (e.g., "cleaned" or "reset") used in user-facing messages.
func runCleanSession(ctx context.Context, cmd *cobra.Command, strat *strategy.ManualCommitStrategy, sessionID string, force, dryRun bool, actionVerb, pastVerb string) error {
	// Verify the session exists
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if dryRun {
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Would %s session %s (phase: %s, checkpoints: %d)\n", strings.ToLower(actionVerb), sessionID, state.Phase, state.StepCount)
		return nil
	}

	if !force {
		var confirmed bool

		title := fmt.Sprintf("%s session %s?", actionVerb, sessionID)
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

	if err := strat.ResetSession(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), sessionID); err != nil {
		return fmt.Errorf("%s session failed: %w", strings.ToLower(actionVerb), err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Session %s has been %s. File changes remain in the working directory.\n", sessionID, pastVerb)
	return nil
}

// runCleanAll cleans all orphaned data across the repository (old `entire clean` behavior).
func runCleanAll(ctx context.Context, cmd *cobra.Command, force, dryRun bool) error {
	// List all cleanup items
	items, err := strategy.ListAllCleanupItems(ctx)
	if err != nil {
		return fmt.Errorf("failed to list orphaned items: %w", err)
	}

	// List temp files
	tempFiles, err := listTempFiles(ctx)
	if err != nil {
		// Non-fatal: continue with other cleanup items
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to list temp files: %v\n", err)
	}

	return runCleanAllWithItems(ctx, cmd, force, dryRun, items, tempFiles)
}

// runCleanAllWithItems is the core logic for cleaning all orphaned items.
// Separated for testability — tests pass a cmd without a TTY and use force or dryRun to avoid prompts.
func runCleanAllWithItems(ctx context.Context, cmd *cobra.Command, force, dryRun bool, items []strategy.CleanupItem, tempFiles []string) error {
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()
	// Handle no items case
	if len(items) == 0 && len(tempFiles) == 0 {
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

	// Show preview when not in force mode
	if !force || dryRun {
		totalItems := len(items) + len(tempFiles)
		fmt.Fprintf(w, "Found %d %s to clean:\n\n", totalItems, itemWord(totalItems))

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

		if len(tempFiles) > 0 {
			fmt.Fprintf(w, "Temp files (%d):\n", len(tempFiles))
			for _, file := range tempFiles {
				fmt.Fprintf(w, "  %s\n", file)
			}
			fmt.Fprintln(w)
		}

		if dryRun {
			fmt.Fprintln(w, "Run without --dry-run to delete these items.")
			return nil
		}

		// Prompt for confirmation
		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Delete %d %s?", totalItems, itemWord(totalItems))).
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

	// Force mode - delete items
	result, err := strategy.DeleteAllCleanupItems(ctx, items)
	if err != nil {
		return fmt.Errorf("failed to delete orphaned items: %w", err)
	}

	// Delete temp files
	deletedTempFiles, failedTempFiles := deleteTempFiles(ctx, tempFiles)

	// Report results
	totalDeleted := len(result.ShadowBranches) + len(result.SessionStates) + len(result.Checkpoints) + len(deletedTempFiles)
	totalFailed := len(result.FailedBranches) + len(result.FailedStates) + len(result.FailedCheckpoints) + len(failedTempFiles)

	if totalDeleted > 0 {
		fmt.Fprintf(w, "✓ Deleted %d %s:\n", totalDeleted, itemWord(totalDeleted))

		if len(result.ShadowBranches) > 0 {
			fmt.Fprintf(w, "\nShadow branches (%d):\n", len(result.ShadowBranches))
			for _, branch := range result.ShadowBranches {
				fmt.Fprintf(w, "  %s\n", branch)
			}
		}

		if len(result.SessionStates) > 0 {
			fmt.Fprintf(w, "\nSession states (%d):\n", len(result.SessionStates))
			for _, state := range result.SessionStates {
				fmt.Fprintf(w, "  %s\n", state)
			}
		}

		if len(result.Checkpoints) > 0 {
			fmt.Fprintf(w, "\nCheckpoints (%d):\n", len(result.Checkpoints))
			for _, cp := range result.Checkpoints {
				fmt.Fprintf(w, "  %s\n", cp)
			}
		}

		if len(deletedTempFiles) > 0 {
			fmt.Fprintf(w, "\nTemp files (%d):\n", len(deletedTempFiles))
			for _, file := range deletedTempFiles {
				fmt.Fprintf(w, "  %s\n", file)
			}
		}
	}

	if totalFailed > 0 {
		fmt.Fprintf(errW, "\nFailed to delete %d %s:\n", totalFailed, itemWord(totalFailed))

		if len(result.FailedBranches) > 0 {
			fmt.Fprintf(errW, "\nShadow branches:\n")
			for _, branch := range result.FailedBranches {
				fmt.Fprintf(errW, "  %s\n", branch)
			}
		}

		if len(result.FailedStates) > 0 {
			fmt.Fprintf(errW, "\nSession states:\n")
			for _, state := range result.FailedStates {
				fmt.Fprintf(errW, "  %s\n", state)
			}
		}

		if len(result.FailedCheckpoints) > 0 {
			fmt.Fprintf(errW, "\nCheckpoints:\n")
			for _, cp := range result.FailedCheckpoints {
				fmt.Fprintf(errW, "  %s\n", cp)
			}
		}

		if len(failedTempFiles) > 0 {
			fmt.Fprintf(errW, "\nTemp files:\n")
			for _, fe := range failedTempFiles {
				fmt.Fprintf(errW, "  %s: %v\n", fe.File, fe.Err)
			}
		}

		return fmt.Errorf("failed to delete %d %s", totalFailed, itemWord(totalFailed))
	}

	return nil
}

// listTempFiles returns files in .entire/tmp/ that are safe to delete,
// excluding files belonging to active sessions.
// Uses os.DirFS + fs.WalkDir to confine listing to the temp directory.
func listTempFiles(ctx context.Context) ([]string, error) {
	absDir, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve temp dir: %w", err)
	}
	root, err := os.OpenRoot(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open root: %w", err)
	}
	defer root.Close()

	// Build set of active session IDs to protect their temp files
	activeSessionIDs := make(map[string]bool)
	if states, listErr := strategy.ListSessionStates(ctx); listErr == nil {
		for _, state := range states {
			activeSessionIDs[state.SessionID] = true
		}
	}

	var files []string
	err = fs.WalkDir(root.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip temp files belonging to active sessions (e.g., "session-id.json")
		name := d.Name()
		sessionID := strings.TrimSuffix(name, ".json")
		if sessionID != name && activeSessionIDs[sessionID] {
			return nil
		}
		files = append(files, name)
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list temp dir: %w", err)
	}
	return files, nil
}

// TempFileDeleteError contains a file name and the error that occurred during deletion.
type TempFileDeleteError struct {
	File string
	Err  error
}

// deleteTempFiles removes all files in .entire/tmp/.
// Uses os.Root to ensure deletions are confined to the temp directory.
// Returns successfully deleted files and any failures with their error reasons.
func deleteTempFiles(ctx context.Context, files []string) (deleted []string, failed []TempFileDeleteError) {
	absDir, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		for _, file := range files {
			failed = append(failed, TempFileDeleteError{File: file, Err: err})
		}
		return nil, failed
	}
	root, err := os.OpenRoot(absDir)
	if err != nil {
		for _, file := range files {
			failed = append(failed, TempFileDeleteError{File: file, Err: err})
		}
		return nil, failed
	}
	defer root.Close()

	for _, file := range files {
		if err := root.Remove(file); err != nil {
			failed = append(failed, TempFileDeleteError{File: file, Err: err})
		} else {
			deleted = append(deleted, file)
		}
	}
	return deleted, failed
}

// activeSessionsOnCurrentHead returns sessions on the current HEAD
// that are in an active phase (ACTIVE).
func activeSessionsOnCurrentHead(ctx context.Context) ([]*session.State, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, err
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}
	currentHead := head.Hash().String()

	states, err := strategy.ListSessionStates(ctx)
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

// itemWord returns "item" or "items" based on count.
func itemWord(n int) string {
	if n == 1 {
		return "item"
	}
	return "items"
}
