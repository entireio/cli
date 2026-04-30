package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

func cleanLongDescription(ctx context.Context) string {
	description := `Clean up Entire session data for the current HEAD commit.

By default, cleans session state and shadow branches for the current HEAD:
  - Session state files (.git/entire-sessions/<session-id>.json)
  - Shadow branch (entire/<commit-hash>-<worktree-hash>)

Use --all to clean all Entire session data across the repository:
  - All session state files (.git/entire-sessions/)
  - All shadow branches
  - Temporary files (.entire/tmp/)`

	s, err := settings.Load(ctx)
	if err == nil && s.IsCheckpointsV2Enabled() {
		description += fmt.Sprintf(`
  - Archived v2 full transcripts older than the configured %d-day retention window`, s.GetFullTranscriptGenerationRetentionDays())
	}

	description += `

Use --session <id> to clean a specific session only.

Without --force, prompts for confirmation before deleting.
Use --dry-run to preview what would be deleted without prompting.`

	return description
}

func newCleanCmd() *cobra.Command {
	var forceFlag bool
	var allFlag bool
	var dryRunFlag bool
	var sessionFlag string

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean up Entire session data",
		Long:  cleanLongDescription(context.Background()),
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
	cmd.Flags().BoolVarP(&allFlag, "all", "a", false, "Clean all session data across the repository")
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

// runCleanAll cleans all session data across the repository.
func runCleanAll(ctx context.Context, cmd *cobra.Command, force, dryRun bool) error {
	s, err := settings.Load(ctx)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to load settings: %v\n", err)
		s = &settings.EntireSettings{}
	}

	// List all items (sessions, shadow branches) — not just orphaned ones
	items, err := strategy.ListAllItems(ctx)
	if err != nil {
		return fmt.Errorf("failed to list items: %w", err)
	}

	if s.IsCheckpointsV2Enabled() {
		v2Items, warnings, err := strategy.ListEligibleV2Generations(ctx, s)
		if err != nil {
			return fmt.Errorf("failed to list v2 generations: %w", err)
		}
		items = append(items, v2Items...)
		for _, warning := range warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", warning)
		}
	}

	// List temp files — skip active-session filter since --all deletes those sessions
	tempFiles, err := listAllTempFiles(ctx)
	if err != nil {
		// Non-fatal: continue with other cleanup items
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to list temp files: %v\n", err)
	}

	return runCleanAllWithItems(ctx, cmd, force, dryRun, items, tempFiles)
}

// printSection prints a titled list of items if the slice is non-empty.
func printSection(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "%s (%d):\n", title, len(items))
	for _, item := range items {
		fmt.Fprintf(w, "  %s\n", item)
	}
	fmt.Fprintln(w)
}

// printResultSection prints a titled list with a leading newline, for post-deletion output.
func printResultSection(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d):\n", title, len(items))
	for _, item := range items {
		fmt.Fprintf(w, "  %s\n", item)
	}
}

// runCleanAllWithItems is the core logic for cleaning all items.
// Separated for testability — tests pass a cmd without a TTY and use force or dryRun to avoid prompts.
func runCleanAllWithItems(ctx context.Context, cmd *cobra.Command, force, dryRun bool, items []strategy.CleanupItem, tempFiles []string) error {
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()
	// Handle no items case
	if len(items) == 0 && len(tempFiles) == 0 {
		fmt.Fprintln(w, "No items to clean up.")
		return nil
	}

	// Group items by type for display
	var branches, states, checkpoints, v2Generations []strategy.CleanupItem
	for _, item := range items {
		switch item.Type {
		case strategy.CleanupTypeShadowBranch:
			branches = append(branches, item)
		case strategy.CleanupTypeSessionState:
			states = append(states, item)
		case strategy.CleanupTypeCheckpoint:
			checkpoints = append(checkpoints, item)
		case strategy.CleanupTypeV2Generation:
			v2Generations = append(v2Generations, item)
		}
	}

	// Show preview when not in force mode
	if !force || dryRun {
		totalItems := len(items) + len(tempFiles)
		fmt.Fprintf(w, "Found %d %s to clean:\n\n", totalItems, itemWord(totalItems))

		printSection(w, "Shadow branches", cleanupItemIDs(branches))
		printSection(w, "Session states", cleanupItemIDs(states))
		printSection(w, "Checkpoint metadata", cleanupItemIDs(checkpoints))
		printSection(w, "Archived v2 generations", cleanupItemIDs(v2Generations))
		printSection(w, "Temp files", tempFiles)

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
		return fmt.Errorf("failed to delete items: %w", err)
	}

	// Delete temp files
	deletedTempFiles, failedTempFiles := deleteTempFiles(ctx, tempFiles)

	// Report results
	totalDeleted := len(result.ShadowBranches) + len(result.SessionStates) + len(result.Checkpoints) + len(result.V2Generations) + len(deletedTempFiles)
	totalFailed := len(result.FailedBranches) + len(result.FailedStates) + len(result.FailedCheckpoints) + len(result.FailedV2Refs) + len(failedTempFiles)

	if totalDeleted > 0 {
		fmt.Fprintf(w, "✓ Deleted %d %s:\n", totalDeleted, itemWord(totalDeleted))

		printResultSection(w, "Shadow branches", result.ShadowBranches)
		printResultSection(w, "Session states", result.SessionStates)
		printResultSection(w, "Checkpoints", result.Checkpoints)
		printResultSection(w, "Archived v2 generations", result.V2Generations)

		printResultSection(w, "Temp files", deletedTempFiles)
	}

	if totalFailed > 0 {
		fmt.Fprintf(errW, "\nFailed to delete %d %s:\n", totalFailed, itemWord(totalFailed))

		printResultSection(errW, "Shadow branches", result.FailedBranches)
		printResultSection(errW, "Session states", result.FailedStates)
		printResultSection(errW, "Checkpoints", result.FailedCheckpoints)
		printResultSection(errW, "Archived v2 generations", result.FailedV2Refs)

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

// cleanupItemIDs extracts IDs from a slice of CleanupItems.
func cleanupItemIDs(items []strategy.CleanupItem) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

// listAllTempFiles returns all files in .entire/tmp/ without filtering.
// Used by --all since those sessions are being deleted anyway.
func listAllTempFiles(ctx context.Context) ([]string, error) {
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

	var files []string
	err = fs.WalkDir(root.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, d.Name())
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
