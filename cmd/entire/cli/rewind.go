package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/charmbracelet/huh"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/spf13/cobra"
)

// unknownSessionID is the fallback session ID used when no session ID is provided.
const unknownSessionID = "unknown"

// getAgent returns an agent by type, falling back to the default agent for empty types.
func getAgent(agentType agentpkg.AgentType) (agentpkg.Agent, error) {
	ag, err := strategy.ResolveAgentForRewind(agentType)
	if err != nil {
		return nil, fmt.Errorf("resolving agent: %w", err)
	}
	return ag, nil
}

func newRewindCmd() *cobra.Command {
	var listFlag bool
	var toFlag string
	var logsOnlyFlag bool
	var resetFlag bool

	cmd := &cobra.Command{
		Use:   "rewind",
		Short: "Browse checkpoints and rewind your session",
		Long: `Interactive command for rewinding and managing agent sessions.

This command will show you an interactive list of recent checkpoints.  You'll be
able to select one for Entire to rewind your branch state, including your code and
your agent's context.`,
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

	return cmd
}

func runRewindInteractive() error { //nolint:maintidx // already present in codebase
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
		fmt.Println("Rewind points are created automatically when agent sessions end.")
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

	// Resolve agent once for use throughout
	agent, err := getAgent(selectedPoint.Agent)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Initialize logging context with agent from checkpoint
	ctx := logging.WithComponent(context.Background(), "rewind")
	ctx = logging.WithAgent(ctx, agent.Name())

	logging.Debug(ctx, "rewind started",
		slog.String("checkpoint_id", selectedPoint.ID),
		slog.String("session_id", selectedPoint.SessionID),
		slog.Bool("is_task_checkpoint", selectedPoint.IsTaskCheckpoint),
	)

	// Perform the rewind using strategy
	if err := start.Rewind(*selectedPoint); err != nil {
		logging.Error(ctx, "rewind failed",
			slog.String("checkpoint_id", selectedPoint.ID),
			slog.String("error", err.Error()),
		)
		return err //nolint:wrapcheck // already present in codebase
	}

	logging.Debug(ctx, "rewind completed",
		slog.String("checkpoint_id", selectedPoint.ID),
	)

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
			if err := restoreTaskCheckpointTranscript(start, *selectedPoint, sessionID, checkpoint.CheckpointUUID, agent); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to restore truncated session transcript: %v\n", err)
			} else {
				fmt.Printf("Rewound to task checkpoint. %s\n", agent.FormatResumeCommand(sessionID))
			}
			return nil
		}
	} else {
		// For session checkpoint: restore full transcript
		// Prefer SessionID from trailer (set by GetRewindPoints from Entire-Session trailer)
		// over path-based extraction which is less reliable.
		sessionID = selectedPoint.SessionID
		if sessionID == "" {
			sessionID = filepath.Base(selectedPoint.MetadataDir)
		}
		transcriptFile = filepath.Join(selectedPoint.MetadataDir, paths.TranscriptFileNameLegacy)
	}

	// Try to restore transcript using the appropriate method:
	// 1. Checkpoint storage (committed checkpoints with valid checkpoint ID)
	// 2. Shadow branch (uncommitted checkpoints with commit hash)
	// 3. Local file (active sessions)
	var restored bool
	if !selectedPoint.CheckpointID.IsEmpty() {
		// Try checkpoint storage first for committed checkpoints
		if returnedSessionID, err := restoreSessionTranscriptFromStrategy(selectedPoint.CheckpointID, sessionID, agent); err == nil {
			sessionID = returnedSessionID
			restored = true
		}
	}

	if !restored && selectedPoint.MetadataDir != "" && len(selectedPoint.ID) == 40 {
		// Try shadow branch for uncommitted checkpoints (ID is a 40-char commit hash)
		if returnedSessionID, err := restoreSessionTranscriptFromShadow(selectedPoint.ID, selectedPoint.MetadataDir, sessionID, agent); err == nil {
			sessionID = returnedSessionID
			restored = true
		}
	}

	if !restored {
		// Fall back to local file
		if err := restoreSessionTranscript(transcriptFile, sessionID, agent); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore session transcript: %v\n", err)
			fmt.Fprintf(os.Stderr, "  Source: %s\n", transcriptFile)
			fmt.Fprintf(os.Stderr, "  Session ID: %s\n", sessionID)
		}
	}

	fmt.Printf("Rewound to %s. %s\n", shortID, agent.FormatResumeCommand(sessionID))
	return nil
}

func runRewindList() error {
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
			CondensationID:   p.CheckpointID.String(),
			SessionID:        p.SessionID,
			SessionPrompt:    p.SessionPrompt,
		}
	}

	// Print as JSON
	data, err := jsonutil.MarshalIndentWithNewline(output, "", "  ")
	if err != nil {
		return err //nolint:wrapcheck // already present in codebase
	}
	fmt.Println(string(data))
	return nil
}

func runRewindToWithOptions(commitID string, logsOnly bool, reset bool) error {
	return runRewindToInternal(commitID, logsOnly, reset)
}

func runRewindToInternal(commitID string, logsOnly bool, reset bool) error {
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

	// Resolve agent once for use throughout
	agent, err := getAgent(selectedPoint.Agent)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Initialize logging context with agent from checkpoint
	ctx := logging.WithComponent(context.Background(), "rewind")
	ctx = logging.WithAgent(ctx, agent.Name())

	logging.Debug(ctx, "rewind started",
		slog.String("checkpoint_id", selectedPoint.ID),
		slog.String("session_id", selectedPoint.SessionID),
		slog.Bool("is_task_checkpoint", selectedPoint.IsTaskCheckpoint),
	)

	// Perform the rewind
	if err := start.Rewind(*selectedPoint); err != nil {
		logging.Error(ctx, "rewind failed",
			slog.String("checkpoint_id", selectedPoint.ID),
			slog.String("error", err.Error()),
		)
		return err //nolint:wrapcheck // already present in codebase
	}

	logging.Debug(ctx, "rewind completed",
		slog.String("checkpoint_id", selectedPoint.ID),
	)

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
			if err := restoreTaskCheckpointTranscript(start, *selectedPoint, sessionID, checkpoint.CheckpointUUID, agent); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to restore truncated session transcript: %v\n", err)
			} else {
				fmt.Printf("Rewound to task checkpoint. %s\n", agent.FormatResumeCommand(sessionID))
			}
			return nil
		}
	} else {
		// Prefer SessionID from trailer over path-based extraction
		sessionID = selectedPoint.SessionID
		if sessionID == "" {
			sessionID = filepath.Base(selectedPoint.MetadataDir)
		}
		transcriptFile = filepath.Join(selectedPoint.MetadataDir, paths.TranscriptFileNameLegacy)
	}

	// Try to restore transcript using the appropriate method:
	// 1. Checkpoint storage (committed checkpoints with valid checkpoint ID)
	// 2. Shadow branch (uncommitted checkpoints with commit hash)
	// 3. Local file (active sessions)
	var restored bool
	if !selectedPoint.CheckpointID.IsEmpty() {
		// Try checkpoint storage first for committed checkpoints
		if returnedSessionID, err := restoreSessionTranscriptFromStrategy(selectedPoint.CheckpointID, sessionID, agent); err == nil {
			sessionID = returnedSessionID
			restored = true
		}
	}

	if !restored && selectedPoint.MetadataDir != "" && len(selectedPoint.ID) == 40 {
		// Try shadow branch for uncommitted checkpoints (ID is a 40-char commit hash)
		if returnedSessionID, err := restoreSessionTranscriptFromShadow(selectedPoint.ID, selectedPoint.MetadataDir, sessionID, agent); err == nil {
			sessionID = returnedSessionID
			restored = true
		}
	}

	if !restored {
		// Fall back to local file
		if err := restoreSessionTranscript(transcriptFile, sessionID, agent); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore session transcript: %v\n", err)
		}
	}

	fmt.Printf("Rewound to %s. %s\n", selectedPoint.ID[:7], agent.FormatResumeCommand(sessionID))
	return nil
}

// handleLogsOnlyRewindNonInteractive handles logs-only rewind in non-interactive mode.
// Defaults to restoring logs only (no checkout) for safety.
func handleLogsOnlyRewindNonInteractive(start strategy.Strategy, point strategy.RewindPoint) error {
	// Resolve agent once for use throughout
	agent, err := getAgent(point.Agent)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Initialize logging context with agent from checkpoint
	ctx := logging.WithComponent(context.Background(), "rewind")
	ctx = logging.WithAgent(ctx, agent.Name())

	logging.Debug(ctx, "logs-only rewind started",
		slog.String("checkpoint_id", point.ID),
		slog.String("session_id", point.SessionID),
	)

	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	sessions, err := restorer.RestoreLogsOnly(point, true) // force=true for explicit rewind
	if err != nil {
		logging.Error(ctx, "logs-only rewind failed",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	logging.Debug(ctx, "logs-only rewind completed",
		slog.String("checkpoint_id", point.ID),
	)

	// Show resume commands for all sessions
	printMultiSessionResumeCommands(sessions)

	fmt.Println("Note: Working directory unchanged. Use interactive mode for full checkout.")
	return nil
}

// handleLogsOnlyResetNonInteractive handles reset in non-interactive mode.
// This performs a git reset --hard to the target commit.
func handleLogsOnlyResetNonInteractive(start strategy.Strategy, point strategy.RewindPoint) error {
	// Resolve agent once for use throughout
	agent, err := getAgent(point.Agent)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Initialize logging context with agent from checkpoint
	ctx := logging.WithComponent(context.Background(), "rewind")
	ctx = logging.WithAgent(ctx, agent.Name())

	logging.Debug(ctx, "logs-only reset started",
		slog.String("checkpoint_id", point.ID),
		slog.String("session_id", point.SessionID),
	)

	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	// Get current HEAD before reset (for recovery message)
	currentHead, headErr := getCurrentHeadHash()
	if headErr != nil {
		currentHead = ""
	}

	// Restore logs first
	sessions, err := restorer.RestoreLogsOnly(point, true) // force=true for explicit rewind
	if err != nil {
		logging.Error(ctx, "logs-only reset failed during log restoration",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	// Perform git reset --hard
	if err := performGitResetHard(point.ID); err != nil {
		logging.Error(ctx, "logs-only reset failed during git reset",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to reset branch: %w", err)
	}

	logging.Debug(ctx, "logs-only reset completed",
		slog.String("checkpoint_id", point.ID),
	)

	shortID := point.ID
	if len(shortID) > 7 {
		shortID = shortID[:7]
	}

	fmt.Printf("Reset branch to %s.\n", shortID)

	// Show resume commands for all sessions
	printMultiSessionResumeCommands(sessions)

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

func restoreSessionTranscript(transcriptFile, sessionID string, agent agentpkg.Agent) error {
	sessionFile, err := resolveTranscriptPath(sessionID, agent)
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o750); err != nil {
		return fmt.Errorf("failed to create agent session directory: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Copying transcript:\n  From: %s\n  To: %s\n", transcriptFile, sessionFile)
	if err := copyFile(transcriptFile, sessionFile); err != nil {
		return fmt.Errorf("failed to copy transcript: %w", err)
	}

	return nil
}

// restoreSessionTranscriptFromStrategy restores a session transcript from checkpoint storage.
// This is used for strategies that store transcripts in git branches rather than local files.
// Returns the session ID that was actually used (may differ from input if checkpoint provides one).
func restoreSessionTranscriptFromStrategy(cpID id.CheckpointID, sessionID string, agent agentpkg.Agent) (string, error) {
	// Get transcript content from checkpoint storage
	content, returnedSessionID, err := checkpoint.LookupSessionLog(cpID)
	if err != nil {
		return "", fmt.Errorf("failed to get session log: %w", err)
	}

	// Use session ID returned from checkpoint if available
	// Otherwise fall back to the passed-in sessionID
	if returnedSessionID != "" {
		sessionID = returnedSessionID
	}

	return writeTranscriptToAgentSession(content, sessionID, agent)
}

// restoreSessionTranscriptFromShadow restores a session transcript from a shadow branch commit.
// This is used for uncommitted checkpoints where the transcript is stored in the shadow branch tree.
func restoreSessionTranscriptFromShadow(commitHash, metadataDir, sessionID string, agent agentpkg.Agent) (string, error) {
	// Open repository
	repo, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", fmt.Errorf("failed to open repository: %w", err)
	}

	// Parse commit hash
	hash := plumbing.NewHash(commitHash)
	if hash.IsZero() {
		return "", fmt.Errorf("invalid commit hash: %s", commitHash)
	}

	// Get transcript from shadow branch commit tree
	store := checkpoint.NewGitStore(repo)
	content, err := store.GetTranscriptFromCommit(hash, metadataDir, agent.Type())
	if err != nil {
		return "", fmt.Errorf("failed to get transcript from shadow branch: %w", err)
	}

	return writeTranscriptToAgentSession(content, sessionID, agent)
}

// writeTranscriptToAgentSession writes transcript content to the agent's session storage.
func writeTranscriptToAgentSession(content []byte, sessionID string, agent agentpkg.Agent) (string, error) {
	sessionFile, err := resolveTranscriptPath(sessionID, agent)
	if err != nil {
		return "", err
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o750); err != nil {
		return "", fmt.Errorf("failed to create agent session directory: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Writing transcript to: %s\n", sessionFile)
	if err := os.WriteFile(sessionFile, content, 0o600); err != nil {
		return "", fmt.Errorf("failed to write transcript: %w", err)
	}

	return sessionID, nil
}

// resolveTranscriptPath determines the correct file path for an agent's session transcript.
// Delegates to strategy.ResolveSessionFilePath after computing the fallback session directory.
func resolveTranscriptPath(sessionID string, agent agentpkg.Agent) (string, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return "", fmt.Errorf("failed to get repository root: %w", err)
	}

	agentSessionDir, err := agent.GetSessionDir(repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to get agent session directory: %w", err)
	}

	return strategy.ResolveSessionFilePath(sessionID, agent, agentSessionDir), nil
}

// restoreTaskCheckpointTranscript restores a truncated transcript for a task checkpoint.
// Uses GetTaskCheckpointTranscript to fetch the transcript from the strategy.
//
// NOTE: The transcript parsing/truncation/writing pipeline (parseTranscriptFromBytes,
// TruncateTranscriptAtUUID, writeTranscript) assumes Claude's JSONL format.
// This is acceptable because task checkpoints are currently only created by Claude Code's
// PostToolUse hook. If other agents gain sub-agent support, this will need a
// format-aware refactor (agent-specific parsing, truncation, and serialization).
func restoreTaskCheckpointTranscript(strat strategy.Strategy, point strategy.RewindPoint, sessionID, checkpointUUID string, agent agentpkg.Agent) error {
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

	sessionFile, err := resolveTranscriptPath(sessionID, agent)
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o750); err != nil {
		return fmt.Errorf("failed to create agent session directory: %w", err)
	}

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
	// Resolve agent once for use throughout
	agent, err := getAgent(point.Agent)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Initialize logging context with agent from checkpoint
	ctx := logging.WithComponent(context.Background(), "rewind")
	ctx = logging.WithAgent(ctx, agent.Name())

	logging.Debug(ctx, "logs-only restore started",
		slog.String("checkpoint_id", point.ID),
		slog.String("session_id", point.SessionID),
	)

	// Check if strategy supports logs-only restoration
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	// Restore logs
	sessions, err := restorer.RestoreLogsOnly(point, true) // force=true for explicit rewind
	if err != nil {
		logging.Error(ctx, "logs-only restore failed",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to restore logs: %w", err)
	}

	logging.Debug(ctx, "logs-only restore completed",
		slog.String("checkpoint_id", point.ID),
	)

	// Show resume commands for all sessions
	fmt.Println("Restored session logs.")
	printMultiSessionResumeCommands(sessions)
	return nil
}

// handleLogsOnlyCheckout restores logs and checks out the commit (detached HEAD).
func handleLogsOnlyCheckout(start strategy.Strategy, point strategy.RewindPoint, shortID string) error {
	// Resolve agent once for use throughout
	agent, err := getAgent(point.Agent)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// Initialize logging context with agent from checkpoint
	ctx := logging.WithComponent(context.Background(), "rewind")
	ctx = logging.WithAgent(ctx, agent.Name())

	logging.Debug(ctx, "logs-only checkout started",
		slog.String("checkpoint_id", point.ID),
		slog.String("session_id", point.SessionID),
	)

	// First, restore the logs
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	sessions, err := restorer.RestoreLogsOnly(point, true) // force=true for explicit rewind
	if err != nil {
		logging.Error(ctx, "logs-only checkout failed during log restoration",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", err.Error()),
		)
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
		printMultiSessionResumeCommands(sessions)
		return nil
	}

	// Perform git checkout
	if err := CheckoutBranch(point.ID); err != nil {
		logging.Error(ctx, "logs-only checkout failed during git checkout",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to checkout commit: %w", err)
	}

	logging.Debug(ctx, "logs-only checkout completed",
		slog.String("checkpoint_id", point.ID),
	)

	fmt.Printf("Checked out %s (detached HEAD).\n", shortID)
	printMultiSessionResumeCommands(sessions)
	return nil
}

// handleLogsOnlyReset restores logs and resets the branch to the commit (destructive).
func handleLogsOnlyReset(start strategy.Strategy, point strategy.RewindPoint, shortID string) error {
	// Resolve agent once for use throughout
	agent, agentErr := getAgent(point.Agent)
	if agentErr != nil {
		return fmt.Errorf("failed to get agent: %w", agentErr)
	}

	// Initialize logging context with agent from checkpoint
	ctx := logging.WithComponent(context.Background(), "rewind")
	ctx = logging.WithAgent(ctx, agent.Name())

	logging.Debug(ctx, "logs-only reset (interactive) started",
		slog.String("checkpoint_id", point.ID),
		slog.String("session_id", point.SessionID),
	)

	// First, restore the logs
	restorer, ok := start.(strategy.LogsOnlyRestorer)
	if !ok {
		return errors.New("strategy does not support logs-only restoration")
	}

	sessions, restoreErr := restorer.RestoreLogsOnly(point, true) // force=true for explicit rewind
	if restoreErr != nil {
		logging.Error(ctx, "logs-only reset failed during log restoration",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", restoreErr.Error()),
		)
		return fmt.Errorf("failed to restore logs: %w", restoreErr)
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
		printMultiSessionResumeCommands(sessions)
		return nil
	}

	// Perform git reset --hard
	if err := performGitResetHard(point.ID); err != nil {
		logging.Error(ctx, "logs-only reset failed during git reset",
			slog.String("checkpoint_id", point.ID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to reset branch: %w", err)
	}

	logging.Debug(ctx, "logs-only reset (interactive) completed",
		slog.String("checkpoint_id", point.ID),
	)

	fmt.Printf("Reset branch to %s.\n", shortID)
	printMultiSessionResumeCommands(sessions)

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

	for count < strategy.MaxCommitTraversalDepth { // Safety limit
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

// printMultiSessionResumeCommands prints resume commands for restored sessions.
// Each session may have a different agent, so per-session agent resolution is used.
func printMultiSessionResumeCommands(sessions []strategy.RestoredSession) {
	if len(sessions) == 0 {
		return
	}

	if len(sessions) > 1 {
		fmt.Printf("\nRestored %d sessions. Resume with:\n", len(sessions))
	}

	for i, sess := range sessions {
		ag, err := strategy.ResolveAgentForRewind(sess.Agent)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not resolve agent %q for session %s, skipping\n", sess.Agent, sess.SessionID)
			continue
		}

		cmd := ag.FormatResumeCommand(sess.SessionID)

		if len(sessions) > 1 {
			// Add "(most recent)" label to the last session
			if i == len(sessions)-1 {
				if sess.Prompt != "" {
					fmt.Printf("  %s  # %s (most recent)\n", cmd, sess.Prompt)
				} else {
					fmt.Printf("  %s  # (most recent)\n", cmd)
				}
			} else {
				if sess.Prompt != "" {
					fmt.Printf("  %s  # %s\n", cmd, sess.Prompt)
				} else {
					fmt.Printf("  %s\n", cmd)
				}
			}
		} else {
			if sess.Prompt != "" {
				fmt.Printf("%s  # %s\n", cmd, sess.Prompt)
			} else {
				fmt.Printf("%s\n", cmd)
			}
		}
	}
}
