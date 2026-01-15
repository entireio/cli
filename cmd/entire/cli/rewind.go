package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/charmbracelet/huh"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/spf13/cobra"
)

// unknownSessionID is the fallback session ID used when no session ID is provided.
const unknownSessionID = "unknown"

func newRewindCmd() *cobra.Command {
	var listFlag bool
	var toFlag string
	var logsOnlyFlag bool
	var resetFlag bool

	cmd := &cobra.Command{
		Use:   "rewind",
		Short: "Rewind commands for session management",
		Long:  "Commands for rewinding and managing Claude Code sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if Entire is disabled
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			if listFlag {
				return runRewindList()
			}
			if toFlag != "" {
				return runRewindToWithOptions(toFlag, logsOnlyFlag, resetFlag)
			}
			return runRewindInteractive()
		},
	}

	cmd.Flags().BoolVar(&listFlag, "list", false, "List available rewind points (JSON output)")
	cmd.Flags().StringVar(&toFlag, "to", "", "Rewind to specific commit ID (non-interactive)")
	cmd.Flags().BoolVar(&logsOnlyFlag, "logs-only", false, "Only restore logs, don't modify working directory (for logs-only points)")
	cmd.Flags().BoolVar(&resetFlag, "reset", false, "Reset branch to commit (destructive, for logs-only points)")

	cmd.AddCommand(newRewindResetCmd())

	return cmd
}

func newRewindResetCmd() *cobra.Command {
	var forceFlag bool

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset the shadow branch for the current HEAD",
		Long: `Discards the shadow branch and session state for the current HEAD commit.

This is useful when:
- A previous session wasn't completed cleanly
- You want to start fresh without existing checkpoints
- Another worktree has a session on the same base commit

The shadow branch (entire/<commit-hash>) and associated session state files
will be deleted. This action cannot be undone.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if Entire is disabled
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			return runRewindReset(forceFlag)
		},
	}

	cmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Skip confirmation prompt")

	return cmd
}

func runRewindReset(force bool) error {
	// Get strategy
	start := GetStrategy()

	// Check if strategy supports reset
	resetter, ok := start.(strategy.SessionResetter)
	if !ok {
		return fmt.Errorf("strategy '%s' does not support reset", start.Name())
	}

	// Delegate to strategy
	if err := resetter.Reset(force); err != nil {
		return fmt.Errorf("reset failed: %w", err)
	}
	return nil
}

func runRewindInteractive() error {
	// Fetch latest entire/sessions from origin
	FetchSessionsBranch()

	// Skip on default branch for strategies that don't allow it
	if skip, _ := ShouldSkipOnDefaultBranchForStrategy(); skip {
		fmt.Println("Entire tracking is disabled on the default branch for this strategy.")
		fmt.Println("Create a feature branch to use Entire's rewind functionality.")
		return nil
	}

	// Get the configured strategy
	start := GetStrategy()

	// Check for uncommitted changes first
	canRewind, changeMsg, err := start.CanRewind()
	if err != nil {
		return fmt.Errorf("failed to check for uncommitted changes: %w", err)
	}
	if !canRewind {
		fmt.Println(changeMsg)
		return nil
	}

	// Get rewind points from strategy
	points, err := start.GetRewindPoints(20)
	if err != nil {
		return fmt.Errorf("failed to find rewind points: %w", err)
	}

	if len(points) == 0 {
		fmt.Println("No rewind points found.")
		fmt.Println("Rewind points are created automatically when Claude Code sessions end.")
		return nil
	}

	// Check if there are multiple sessions (to show session identifier)
	sessionIDs := make(map[string]bool)
	for _, p := range points {
		if p.SessionID != "" {
			sessionIDs[p.SessionID] = true
		}
	}
	hasMultipleSessions := len(sessionIDs) > 1

	// Build options for the select menu
	options := make([]huh.Option[string], 0, len(points)+1)
	for _, p := range points {
		var label string
		timestamp := p.Date.Format("2006-01-02 15:04")

		// Build session identifier for display when multiple sessions exist
		sessionLabel := ""
		if hasMultipleSessions && p.SessionPrompt != "" {
			// Show truncated prompt to identify the session
			sessionLabel = fmt.Sprintf(" [%s]", sanitizeForTerminal(p.SessionPrompt))
		}

		switch {
		case p.IsLogsOnly:
			// Committed checkpoint - show commit sha (this is the real user commit)
			shortID := p.ID
			if len(shortID) >= 7 {
				shortID = shortID[:7]
			}
			label = fmt.Sprintf("%s (%s) %s%s", shortID, timestamp, sanitizeForTerminal(p.Message), sessionLabel)
		case p.IsTaskCheckpoint:
			// Task checkpoint (uncommitted) - no sha shown
			label = fmt.Sprintf("        (%s) [Task] %s%s", timestamp, sanitizeForTerminal(p.Message), sessionLabel)
		default:
			// Shadow checkpoint (uncommitted) - no sha shown (internal commit)
			label = fmt.Sprintf("        (%s) %s%s", timestamp, sanitizeForTerminal(p.Message), sessionLabel)
		}
		options = append(options, huh.NewOption(label, p.ID))
	}
	options = append(options, huh.NewOption("Cancel", "cancel"))

	var selectedID string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select a checkpoint to restore").
				Description("Your working directory will be restored to this checkpoint's state").
				Options(options...).
				Value(&selectedID),
		),
	)

	if err := form.Run(); err != nil {
		return fmt.Errorf("selection cancelled: %w", err)
	}

	if selectedID == "cancel" {
		fmt.Println("Rewind cancelled.")
		return nil
	}

	// Find the selected point
	var selectedPoint *strategy.RewindPoint
	for _, p := range points {
		if p.ID == selectedID {
			pointCopy := p
			selectedPoint = &pointCopy
			break
		}
	}

	if selectedPoint == nil {
		return errors.New("rewind point not found")
	}

	shortID := selectedPoint.ID
	if len(shortID) > 7 {
		shortID = shortID[:7]
	}

	// Show what was selected
	switch {
	case selectedPoint.IsLogsOnly:
		// Committed checkpoint - show sha
		fmt.Printf("\nSelected: %s %s\n", shortID, sanitizeForTerminal(selectedPoint.Message))
	case selectedPoint.IsTaskCheckpoint:
		// Task checkpoint - no sha
		fmt.Printf("\nSelected: [Task] %s\n", sanitizeForTerminal(selectedPoint.Message))
	default:
		// Shadow checkpoint - no sha
		fmt.Printf("\nSelected: %s\n", sanitizeForTerminal(selectedPoint.Message))
	}

	// Handle logs-only points with a sub-choice menu
	if selectedPoint.IsLogsOnly {
		return handleLogsOnlyRewindInteractive(start, *selectedPoint, shortID)
	}

	// Preview rewind to show warnings about files that will be deleted
	preview, previewErr := start.PreviewRewind(*selectedPoint)
	if previewErr == nil && preview != nil && len(preview.FilesToDelete) > 0 {
		fmt.Fprintf(os.Stderr, "\nWarning: The following untracked files will be DELETED:\n")
		for _, f := range preview.FilesToDelete {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		fmt.Fprintf(os.Stderr, "\n")
	}

	// Confirm rewind
	var confirm bool
	description := fmt.Sprintf("This will reset to: %s\nChanges after this point may be lost!", selectedPoint.Message)
	confirmForm := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Reset to %s?", shortID)).
				Description(description).
				Value(&confirm),
		),
	)

	if err := confirmForm.Run(); err != nil {
		return fmt.Errorf("confirmation cancelled: %w", err)
	}

	if !confirm {
		fmt.Println("Rewind cancelled.")
		return nil
	}

	// Perform the rewind using strategy
	if err := start.Rewind(*selectedPoint); err != nil {
		return err
	}

	// Handle transcript restoration differently for task checkpoints
	var sessionID string
	var transcriptFile string

	if selectedPoint.IsTaskCheckpoint {
		// For task checkpoint: read checkpoint.json to get UUID and truncate transcript
		checkpoint, err := start.GetTaskCheckpoint(*selectedPoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to read task checkpoint: %v\n", err)
			return nil
		}

		sessionID = checkpoint.SessionID

		if checkpoint.CheckpointUUID != "" {
			// Truncate transcript at checkpoint UUID
			if err := restoreTaskCheckpointTranscript(start, *selectedPoint, sessionID, checkpoint.CheckpointUUID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to restore truncated session transcript: %v\n", err)
			} else {
				fmt.Printf("Rewound to task checkpoint. %s\n", formatResumeCommand(sessionID))
			}
			return nil
		}
	} else {
		// For session checkpoint: restore full transcript
		sessionID = extractSessionIDFromMetadata(selectedPoint.MetadataDir)
		transcriptFile = filepath.Join(selectedPoint.MetadataDir, paths.TranscriptFileNameLegacy)
	}

	// Try to restore using GetSessionLog first (for strategies that store transcripts in git)
	// Use checkpoint ID if available (auto-commit strategy), otherwise use commit ID (manual-commit strategy)
	lookupID := selectedPoint.CheckpointID
	if lookupID == "" {
		lookupID = selectedPoint.ID
	}
	if returnedSessionID, err := restoreSessionTranscriptFromStrategy(start, lookupID, sessionID); err != nil {
		// Fall back to local file
		if err := restoreSessionTranscript(transcriptFile, sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore session transcript: %v\n", err)
			fmt.Fprintf(os.Stderr, "  Source: %s\n", transcriptFile)
			fmt.Fprintf(os.Stderr, "  Session ID: %s\n", sessionID)
		}
	} else {
		// Use session ID returned from strategy (may differ from what we extracted from metadata path)
		sessionID = returnedSessionID
	}

	fmt.Printf("Rewound to %s. %s\n", shortID, formatResumeCommand(sessionID))
	return nil
}

func runRewindList() error {
	// Fetch latest entire/sessions from origin
	FetchSessionsBranch()

	// Skip on default branch for strategies that don't allow it
	if skip, _ := ShouldSkipOnDefaultBranchForStrategy(); skip {
		// Return empty list for programmatic consumers
		fmt.Println("[]")
		return nil
	}

	start := GetStrategy()

	points, err := start.GetRewindPoints(20)
	if err != nil {
		return fmt.Errorf("failed to find rewind points: %w", err)
	}

	// Output as JSON for programmatic use
	type jsonPoint struct {
		ID               string `json:"id"`
		Message          string `json:"message"`
		MetadataDir      string `json:"metadata_dir"`
		Date             string `json:"date"`
		IsTaskCheckpoint bool   `json:"is_task_checkpoint"`
		ToolUseID        string `json:"tool_use_id,omitempty"`
		IsLogsOnly       bool   `json:"is_logs_only"`
		CondensationID   string `json:"condensation_id,omitempty"`
		SessionID        string `json:"session_id,omitempty"`
		SessionPrompt    string `json:"session_prompt,omitempty"`
	}

	output := make([]jsonPoint, len(points))
	for i, p := range points {
		output[i] = jsonPoint{
			ID:               p.ID,
			Message:          p.Message,
			MetadataDir:      p.MetadataDir,
			Date:             p.Date.Format(time.RFC3339),
			IsTaskCheckpoint: p.IsTaskCheckpoint,
			ToolUseID:        p.ToolUseID,
			IsLogsOnly:       p.IsLogsOnly,
			CondensationID:   p.CheckpointID,
			SessionID:        p.SessionID,
			SessionPrompt:    p.SessionPrompt,
		}
	}

	// Print as JSON
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func runRewindToWithOptions(commitID string, logsOnly bool, reset bool) error {
	return runRewindToInternal(commitID, logsOnly, reset)
}

func runRewindToInternal(commitID string, logsOnly bool, reset bool) error {
	// Fetch latest entire/sessions from origin
	FetchSessionsBranch()

	// Skip on default branch for strategies that don't allow it
	if skip, _ := ShouldSkipOnDefaultBranchForStrategy(); skip {
		return errors.New("entire tracking is disabled on the default branch for this strategy - create a feature branch to use rewind")
	}

	start := GetStrategy()

	// Check for uncommitted changes (skip for reset which handles this itself)
	if !reset {
		canRewind, changeMsg, err := start.CanRewind()
		if err != nil {
			return fmt.Errorf("failed to check for uncommitted changes: %w", err)
		}
		if !canRewind {
			return fmt.Errorf("%s", changeMsg)
		}
	}

	// Get rewind points
	points, err := start.GetRewindPoints(20)
	if err != nil {
		return fmt.Errorf("failed to find rewind points: %w", err)
	}

	// Find the matching point (support both full and short commit IDs)
	var selectedPoint *strategy.RewindPoint
	for _, p := range points {
		if p.ID == commitID || (len(commitID) >= 7 && len(p.ID) >= 7 && strings.HasPrefix(p.ID, commitID)) {
			pointCopy := p
			selectedPoint = &pointCopy
			break
		}
	}

	if selectedPoint == nil {
		return fmt.Errorf("rewind point not found: %s", commitID)
	}

	// Handle reset mode (for logs-only points)
	if reset {
		return handleLogsOnlyResetNonInteractive(start, *selectedPoint)
	}

	// Handle logs-only restoration:
	// 1. For logs-only points, always use logs-only restoration
	// 2. If --logs-only flag is set, use logs-only restoration even for checkpoint points
	if selectedPoint.IsLogsOnly || logsOnly {
		return handleLogsOnlyRewindNonInteractive(start, *selectedPoint)
	}

	// Preview rewind to show warnings about files that will be deleted
	preview, previewErr := start.PreviewRewind(*selectedPoint)
	if previewErr == nil && preview != nil && len(preview.FilesToDelete) > 0 {
		fmt.Fprintf(os.Stderr, "\nWarning: The following untracked files will be DELETED:\n")
		for _, f := range preview.FilesToDelete {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		fmt.Fprintf(os.Stderr, "\n")
	}

	// Perform the rewind
	if err := start.Rewind(*selectedPoint); err != nil {
		return err
	}

	// Handle transcript restoration
	var sessionID string
	var transcriptFile string

	if selectedPoint.IsTaskCheckpoint {
		checkpoint, err := start.GetTaskCheckpoint(*selectedPoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to read task checkpoint: %v\n", err)
			return nil
		}

		sessionID = checkpoint.SessionID

		if checkpoint.CheckpointUUID != "" {
			// Use strategy-based transcript restoration for task checkpoints
			if err := restoreTaskCheckpointTranscript(start, *selectedPoint, sessionID, checkpoint.CheckpointUUID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to restore truncated session transcript: %v\n", err)
			} else {
				fmt.Printf("Rewound to task checkpoint. %s\n", formatResumeCommand(sessionID))
			}
			return nil
		}
	} else {
		sessionID = extractSessionIDFromMetadata(selectedPoint.MetadataDir)
		transcriptFile = filepath.Join(selectedPoint.MetadataDir, paths.TranscriptFileNameLegacy)
	}

	// Try to restore using GetSessionLog first (for strategies that store transcripts in git)
	// Use checkpoint ID if available (auto-commit strategy), otherwise use commit ID (manual-commit strategy)
	lookupID := selectedPoint.CheckpointID
	if lookupID == "" {
		lookupID = selectedPoint.ID
	}
	if returnedSessionID, err := restoreSessionTranscriptFromStrategy(start, lookupID, sessionID); err != nil {
		// Fall back to local file
		if err := restoreSessionTranscript(transcriptFile, sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore session transcript: %v\n", err)
		}
	} else {
		// Use session ID returned from strategy (may differ from what we extracted from metadata path)
		sessionID = returnedSessionID
	}

	fmt.Printf("Rewound to %s. %s\n", selectedPoint.ID[:7], formatResumeCommand(sessionID))
	return nil
}

// handleLogsOnlyRewindNonInteractive handles logs-only rewind in non-interactive mode.
// Defaults to restoring logs only (no checkout) for safety.
func handleLogsOnlyRewindNonInteractive(start strategy.Strategy, point strategy.RewindPoint) error {
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	if err := restorer.RestoreLogsOnly(point, true); err != nil { // force=true for explicit rewind
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	// Show resume commands for all sessions
	printMultiSessionResumeCommands(point)

	fmt.Println("Note: Working directory unchanged. Use interactive mode for full checkout.")
	return nil
}

// handleLogsOnlyResetNonInteractive handles reset in non-interactive mode.
// This performs a git reset --hard to the target commit.
func handleLogsOnlyResetNonInteractive(start strategy.Strategy, point strategy.RewindPoint) error {
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	// Get current HEAD before reset (for recovery message)
	currentHead, err := getCurrentHeadHash()
	if err != nil {
		currentHead = ""
	}

	// Restore logs first
	if err := restorer.RestoreLogsOnly(point, true); err != nil { // force=true for explicit rewind
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	// Perform git reset --hard
	if err := performGitResetHard(point.ID); err != nil {
		return fmt.Errorf("failed to reset branch: %w", err)
	}

	shortID := point.ID
	if len(shortID) > 7 {
		shortID = shortID[:7]
	}

	fmt.Printf("Reset branch to %s.\n", shortID)

	// Show resume commands for all sessions
	printMultiSessionResumeCommands(point)

	// Show recovery instructions
	if currentHead != "" && currentHead != point.ID {
		currentShort := currentHead
		if len(currentShort) > 7 {
			currentShort = currentShort[:7]
		}
		fmt.Printf("\nTo undo this reset: git reset --hard %s\n", currentShort)
	}

	return nil
}

func extractSessionIDFromMetadata(metadataDir string) string {
	// Metadata dir format varies by strategy:
	// - manual-commit/commit: .entire/metadata/2025-01-25-<session-id>
	// - auto-commit: ab/cdef1234.../2025-11-28-<session-id>
	base := filepath.Base(metadataDir)
	// Try to extract UUID from the end (format: YYYY-MM-DD-uuid)
	parts := strings.SplitN(base, "-", 4)
	if len(parts) >= 4 {
		return parts[3]
	}
	return base
}

func restoreSessionTranscript(transcriptFile, sessionID string) error {
	// Get the agent for session directory and ID transformation
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Get repo root for agent's session directory lookup
	// Use repo root instead of CWD because Claude stores sessions per-repo,
	// and running from a subdirectory would look up the wrong session directory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	// Get agent's session storage directory
	sessionDir, err := ag.GetSessionDir(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to get agent session directory: %w", err)
	}

	// Ensure session directory exists
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		return fmt.Errorf("failed to create agent session directory: %w", err)
	}

	// Extract agent's session ID format from Entire session ID
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	sessionFile := filepath.Join(sessionDir, agentSessionID+".jsonl")
	fmt.Fprintf(os.Stderr, "Copying transcript:\n  From: %s\n  To: %s\n", transcriptFile, sessionFile)
	if err := copyFile(transcriptFile, sessionFile); err != nil {
		return fmt.Errorf("failed to copy transcript: %w", err)
	}

	return nil
}

// restoreSessionTranscriptFromStrategy restores a session transcript using GetSessionLog.
// This is used for strategies that store transcripts in git branches rather than local files.
// Returns the session ID that was actually used (may differ from input if strategy provides one).
func restoreSessionTranscriptFromStrategy(strat strategy.Strategy, checkpointID, sessionID string) (string, error) {
	// Get the agent for session directory and ID transformation
	ag, err := GetAgent()
	if err != nil {
		return "", fmt.Errorf("failed to get agent: %w", err)
	}

	// Get repo root for agent's session directory lookup
	// Use repo root instead of CWD because Claude stores sessions per-repo,
	// and running from a subdirectory would look up the wrong session directory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return "", fmt.Errorf("failed to get repository root: %w", err)
	}

	// Get agent's session storage directory
	agentSessionDir, err := ag.GetSessionDir(repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to get agent session directory: %w", err)
	}

	// Ensure session directory exists
	if err := os.MkdirAll(agentSessionDir, 0o750); err != nil {
		return "", fmt.Errorf("failed to create agent session directory: %w", err)
	}

	// Get transcript content from strategy
	content, returnedSessionID, err := strat.GetSessionLog(checkpointID)
	if err != nil {
		return "", fmt.Errorf("failed to get session log: %w", err)
	}

	// Use session ID returned from strategy if available (some strategies store it in git)
	// Otherwise fall back to the passed-in sessionID
	if returnedSessionID != "" {
		sessionID = returnedSessionID
	}

	// Write transcript to agent's session storage
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	sessionFile := filepath.Join(agentSessionDir, agentSessionID+".jsonl")
	fmt.Fprintf(os.Stderr, "Writing transcript to: %s\n", sessionFile)
	if err := os.WriteFile(sessionFile, content, 0o600); err != nil {
		return "", fmt.Errorf("failed to write transcript: %w", err)
	}

	return sessionID, nil
}

// restoreTaskCheckpointTranscript restores a truncated transcript for a task checkpoint.
// Uses GetTaskCheckpointTranscript to fetch the transcript from the strategy.
func restoreTaskCheckpointTranscript(strat strategy.Strategy, point strategy.RewindPoint, sessionID, checkpointUUID string) error {
	// Get the agent for session directory and ID transformation
	ag, err := GetAgent()
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Get transcript content from strategy
	content, err := strat.GetTaskCheckpointTranscript(point)
	if err != nil {
		return fmt.Errorf("failed to get task checkpoint transcript: %w", err)
	}

	// Parse the transcript
	transcript, err := parseTranscriptFromBytes(content)
	if err != nil {
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Truncate at checkpoint UUID
	truncated := TruncateTranscriptAtUUID(transcript, checkpointUUID)

	// Get repo root for agent's session directory lookup
	// Use repo root instead of CWD because Claude stores sessions per-repo,
	// and running from a subdirectory would look up the wrong session directory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	// Get agent's session storage directory
	agentSessionDir, err := ag.GetSessionDir(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to get agent session directory: %w", err)
	}

	// Ensure session directory exists
	if err := os.MkdirAll(agentSessionDir, 0o750); err != nil {
		return fmt.Errorf("failed to create agent session directory: %w", err)
	}

	// Write truncated transcript to agent's session storage
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	sessionFile := filepath.Join(agentSessionDir, agentSessionID+".jsonl")
	fmt.Fprintf(os.Stderr, "Writing truncated transcript to: %s\n", sessionFile)

	if err := writeTranscript(sessionFile, truncated); err != nil {
		return fmt.Errorf("failed to write truncated transcript: %w", err)
	}

	return nil
}

// createContextFileMinimal creates a context file without staged files info
// (since the strategy will handle staging)
func createContextFileMinimal(contextFile, commitMessage, sessionID, promptFile, summaryFile string, transcript []transcriptLine) error {
	prompt, _ := os.ReadFile(promptFile)   //nolint:errcheck,gosec // Best-effort loading of optional context files
	summary, _ := os.ReadFile(summaryFile) //nolint:errcheck,gosec // Best-effort loading of optional context files
	keyActions := extractKeyActions(transcript, 10)

	var content strings.Builder
	content.WriteString("# Session Context\n\n")
	content.WriteString(fmt.Sprintf("**Session ID:** %s\n\n", sessionID))
	content.WriteString(fmt.Sprintf("**Commit Message:** %s\n\n", commitMessage))
	content.WriteString("## Prompt\n\n")
	content.Write(prompt)
	content.WriteString("\n\n## Summary\n\n")
	content.Write(summary)
	content.WriteString("\n\n## Key Actions\n\n")
	for _, action := range keyActions {
		content.WriteString(fmt.Sprintf("- %s\n", action))
	}

	if err := os.WriteFile(contextFile, []byte(content.String()), 0o600); err != nil {
		return fmt.Errorf("failed to write context file: %w", err)
	}
	return nil
}

// handleLogsOnlyRewindInteractive handles rewind for logs-only points with a sub-choice menu.
func handleLogsOnlyRewindInteractive(start strategy.Strategy, point strategy.RewindPoint, shortID string) error {
	var action string

	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Logs-only point: "+shortID).
				Description("This commit has session logs but no checkpoint state. Choose an action:").
				Options(
					huh.NewOption("Restore logs only (keep current files)", "logs"),
					huh.NewOption("Checkout commit (detached HEAD, for viewing)", "checkout"),
					huh.NewOption("Reset branch to this commit (destructive!)", "reset"),
					huh.NewOption("Cancel", "cancel"),
				).
				Value(&action),
		),
	)

	if err := form.Run(); err != nil {
		return fmt.Errorf("action selection cancelled: %w", err)
	}

	switch action {
	case "logs":
		return handleLogsOnlyRestore(start, point)
	case "checkout":
		return handleLogsOnlyCheckout(start, point, shortID)
	case "reset":
		return handleLogsOnlyReset(start, point, shortID)
	case "cancel":
		fmt.Println("Rewind cancelled.")
		return nil
	}

	return nil
}

// handleLogsOnlyRestore restores only the session logs without changing files.
func handleLogsOnlyRestore(start strategy.Strategy, point strategy.RewindPoint) error {
	// Check if strategy supports logs-only restoration
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	// Restore logs
	if err := restorer.RestoreLogsOnly(point, true); err != nil { // force=true for explicit rewind
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	// Show resume commands for all sessions
	fmt.Println("Restored session logs.")
	printMultiSessionResumeCommands(point)
	return nil
}

// handleLogsOnlyCheckout restores logs and checks out the commit (detached HEAD).
func handleLogsOnlyCheckout(start strategy.Strategy, point strategy.RewindPoint, shortID string) error {
	// First, restore the logs
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	if err := restorer.RestoreLogsOnly(point, true); err != nil { // force=true for explicit rewind
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	// Show warning about detached HEAD
	var confirm bool
	confirmForm := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Create detached HEAD?").
				Description("This will checkout the commit directly. You'll be in 'detached HEAD' state.\nAny uncommitted changes will be lost!").
				Value(&confirm),
		),
	)

	if err := confirmForm.Run(); err != nil {
		return fmt.Errorf("confirmation cancelled: %w", err)
	}

	if !confirm {
		fmt.Println("Checkout cancelled. Session logs were still restored.")
		printMultiSessionResumeCommands(point)
		return nil
	}

	// Perform git checkout
	if err := CheckoutBranch(point.ID); err != nil {
		return fmt.Errorf("failed to checkout commit: %w", err)
	}

	fmt.Printf("Checked out %s (detached HEAD).\n", shortID)
	printMultiSessionResumeCommands(point)
	return nil
}

// handleLogsOnlyReset restores logs and resets the branch to the commit (destructive).
func handleLogsOnlyReset(start strategy.Strategy, point strategy.RewindPoint, shortID string) error {
	// First, restore the logs
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	if err := restorer.RestoreLogsOnly(point, true); err != nil { // force=true for explicit rewind
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	// Get current HEAD before reset (for recovery message)
	currentHead, err := getCurrentHeadHash()
	if err != nil {
		// Non-fatal - just won't show recovery message
		currentHead = ""
	}

	// Get detailed uncommitted changes warning from strategy
	var uncommittedWarning string
	if _, warn, err := start.CanRewind(); err == nil {
		uncommittedWarning = warn
	}

	// Check for safety issues
	warnings, err := checkResetSafety(point.ID, uncommittedWarning)
	if err != nil {
		return fmt.Errorf("failed to check reset safety: %w", err)
	}

	// Build confirmation message based on warnings
	var confirmTitle, confirmDesc string
	if len(warnings) > 0 {
		confirmTitle = "⚠️  Reset branch with warnings?"
		confirmDesc = "WARNING - the following issues were detected:\n" +
			strings.Join(warnings, "\n") +
			"\n\nThis will move your branch to " + shortID + " and DISCARD commits after it!"
	} else {
		confirmTitle = "Reset branch to " + shortID + "?"
		confirmDesc = "This will move your branch pointer to this commit.\nCommits after this point will be orphaned (but recoverable via reflog)."
	}

	var confirm bool
	confirmForm := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(confirmTitle).
				Description(confirmDesc).
				Value(&confirm),
		),
	)

	if err := confirmForm.Run(); err != nil {
		return fmt.Errorf("confirmation cancelled: %w", err)
	}

	if !confirm {
		fmt.Println("Reset cancelled. Session logs were still restored.")
		printMultiSessionResumeCommands(point)
		return nil
	}

	// Perform git reset --hard
	if err := performGitResetHard(point.ID); err != nil {
		return fmt.Errorf("failed to reset branch: %w", err)
	}

	fmt.Printf("Reset branch to %s.\n", shortID)
	printMultiSessionResumeCommands(point)

	// Show recovery instructions
	if currentHead != "" && currentHead != point.ID {
		currentShort := currentHead
		if len(currentShort) > 7 {
			currentShort = currentShort[:7]
		}
		fmt.Printf("\nTo undo this reset: git reset --hard %s\n", currentShort)
	}

	return nil
}

// getCurrentHeadHash returns the current HEAD commit hash.
func getCurrentHeadHash() (string, error) {
	repo, err := openRepository()
	if err != nil {
		return "", err
	}

	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	return head.Hash().String(), nil
}

// checkResetSafety checks for potential issues before a git reset --hard.
// Returns a list of warning messages (empty if safe to proceed without warnings).
// If uncommittedChangesWarning is provided, it will be used instead of a generic warning.
func checkResetSafety(targetCommitHash string, uncommittedChangesWarning string) ([]string, error) {
	var warnings []string

	repo, err := openRepository()
	if err != nil {
		return nil, err
	}

	// Check for uncommitted changes
	if uncommittedChangesWarning != "" {
		// Use the detailed warning from strategy's CanRewind()
		warnings = append(warnings, uncommittedChangesWarning)
	} else {
		// Fall back to generic check
		worktree, err := repo.Worktree()
		if err != nil {
			return nil, fmt.Errorf("failed to get worktree: %w", err)
		}

		status, err := worktree.Status()
		if err != nil {
			return nil, fmt.Errorf("failed to get status: %w", err)
		}

		if !status.IsClean() {
			warnings = append(warnings, "• You have uncommitted changes that will be LOST")
		}
	}

	// Check if current HEAD is ahead of target (we'd be discarding commits)
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	targetHash := plumbing.NewHash(targetCommitHash)

	// Count commits between target and HEAD
	commitsAhead, err := countCommitsBetween(repo, targetHash, head.Hash())
	if err != nil {
		// Non-fatal - just can't show commit count
		commitsAhead = -1
	}

	if commitsAhead > 0 {
		warnings = append(warnings, fmt.Sprintf("• %d commit(s) after this point will be orphaned", commitsAhead))
	}

	return warnings, nil
}

// countCommitsBetween counts commits between ancestor and descendant.
// Returns 0 if ancestor == descendant, -1 on error.
func countCommitsBetween(repo *git.Repository, ancestor, descendant plumbing.Hash) (int, error) {
	if ancestor == descendant {
		return 0, nil
	}

	// Walk from descendant back to ancestor
	count := 0
	current := descendant

	for count < 1000 { // Safety limit
		if current == ancestor {
			return count, nil
		}

		commit, err := repo.CommitObject(current)
		if err != nil {
			return -1, fmt.Errorf("failed to get commit: %w", err)
		}

		if commit.NumParents() == 0 {
			// Reached root without finding ancestor - ancestor not in history
			return -1, nil
		}

		count++
		current = commit.ParentHashes[0] // Follow first parent
	}

	return -1, nil
}

// performGitResetHard performs a git reset --hard to the specified commit.
// Uses the git CLI instead of go-git because go-git's HardReset incorrectly
// deletes untracked directories (like .entire/) even when they're in .gitignore.
func performGitResetHard(commitHash string) error {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "reset", "--hard", commitHash)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reset failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// extractSessionIDFromLogsOnlyPoint extracts the session ID from a logs-only point.
func extractSessionIDFromLogsOnlyPoint(start strategy.Strategy, point strategy.RewindPoint) string {
	// Try to get from condensation metadata via strategy
	if sv, ok := start.(*strategy.ManualCommitStrategy); ok {
		// The getSessionIDFromCondensation method is not exported, so we'll
		// extract from the commit's Entire-Session trailer instead
		_ = sv // Avoid unused variable error
	}

	// Extract from commit's Entire-Session trailer
	repo, err := openRepository()
	if err != nil {
		return ""
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(point.ID))
	if err != nil {
		return ""
	}

	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return ""
	}

	// Parse Entire-Session trailer
	sessionID, found := paths.ParseSessionTrailer(commit.Message)
	if found {
		return sessionID
	}

	return ""
}

// sanitizeForTerminal removes or replaces characters that cause rendering issues
// in terminal UI components. This includes emojis with skin-tone modifiers and
// other multi-codepoint characters that confuse width calculations.
func sanitizeForTerminal(s string) string {
	var result strings.Builder
	result.Grow(len(s))

	for _, r := range s {
		// Skip emoji skin tone modifiers (U+1F3FB to U+1F3FF)
		if r >= 0x1F3FB && r <= 0x1F3FF {
			continue
		}
		// Skip zero-width joiners used in emoji sequences
		if r == 0x200D {
			continue
		}
		// Skip variation selectors (U+FE00 to U+FE0F)
		if r >= 0xFE00 && r <= 0xFE0F {
			continue
		}
		// Keep printable characters and common whitespace
		if unicode.IsPrint(r) || r == '\t' || r == '\n' {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// formatResumeCommand returns the agent-appropriate command to resume a session.
// Falls back to a generic format if the agent cannot be loaded.
func formatResumeCommand(entireSessionID string) string {
	ag, err := GetAgent()
	if err != nil {
		// Fallback to generic format
		return "Resume session: " + entireSessionID
	}
	agentSessionID := ag.ExtractAgentSessionID(entireSessionID)
	return ag.FormatResumeCommand(agentSessionID)
}

// printMultiSessionResumeCommands prints resume commands for all sessions in a rewind point.
// For single-session checkpoints, prints a single resume command.
// For multi-session checkpoints, prints all sessions with prompts as comments.
func printMultiSessionResumeCommands(point strategy.RewindPoint) {
	// Check if this is a multi-session checkpoint
	if point.SessionCount > 1 && len(point.SessionIDs) > 1 {
		fmt.Printf("\nRestored %d sessions. Resume with:\n", point.SessionCount)

		// Print each session with prompt as comment
		for i, sessionID := range point.SessionIDs {
			cmd := formatResumeCommand(sessionID)

			// Get prompt for this session (if available)
			var prompt string
			if i < len(point.SessionPrompts) {
				prompt = point.SessionPrompts[i]
			}

			// Add "(most recent)" label to the last session
			if i == len(point.SessionIDs)-1 {
				if prompt != "" {
					fmt.Printf("  %s  # %s (most recent)\n", cmd, prompt)
				} else {
					fmt.Printf("  %s  # (most recent)\n", cmd)
				}
			} else {
				if prompt != "" {
					fmt.Printf("  %s  # %s\n", cmd, prompt)
				} else {
					fmt.Printf("  %s\n", cmd)
				}
			}
		}
	} else {
		// Single session - use traditional format with prompt if available
		sessionID := point.SessionID
		if sessionID == "" && len(point.SessionIDs) > 0 {
			sessionID = point.SessionIDs[len(point.SessionIDs)-1]
		}
		if sessionID != "" {
			cmd := formatResumeCommand(sessionID)
			if point.SessionPrompt != "" {
				fmt.Printf("%s  # %s\n", cmd, point.SessionPrompt)
			} else {
				fmt.Printf("%s\n", cmd)
			}
		}
	}
}
