package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/charmbracelet/huh"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/spf13/cobra"
)

const (
	// maxCommitsToScanForCheckpointsUntilMainBranch limits commit traversal when
	// searching for checkpoint trailers. Traversal stops at main/master branch or
	// after this many commits, whichever comes first.
	maxCommitsToScanForCheckpointsUntilMainBranch = 50

	// sessionPickerCancelValue is the value used for the cancel option in session pickers
	sessionPickerCancelValue = "cancel"
)

// Trailer keys for session linking - use paths package constants
// paths.SessionTrailerKey = "Entire-Session"
// paths.SourceRefTrailerKey = "Entire-Source-Ref"

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Session information and management",
		Long:  "Commands for viewing and managing AI session information",
	}

	cmd.AddCommand(newSessionRawCmd())
	cmd.AddCommand(newSessionListCmd())
	cmd.AddCommand(newSessionResumeCmd())
	cmd.AddCommand(newSessionCurrentCmd())
	cmd.AddCommand(newCleanCmd())

	return cmd
}

func newSessionRawCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "raw <commit>",
		Short: "Output raw session transcript",
		Long:  "Output the session transcript for a session by commit hash",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runSessionRaw(args[0])
		},
	}

	return cmd
}

func runSessionRaw(commitRef string) error {
	start := GetStrategy()
	// First try to get the log directly via the strategy
	content, _, err := start.GetSessionLog(commitRef)
	if err == nil {
		fmt.Print(string(content))
		return nil
	}

	// If that fails, try to resolve through source ref trailers
	if !errors.Is(err, strategy.ErrNoMetadata) {
		return err
	}

	// Try to find session via trailers
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(commitRef))
	if err != nil {
		return fmt.Errorf("failed to resolve %s: %w", commitRef, err)
	}

	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}

	_, sourceRef := parseSessionTrailers(commit.Message)
	if sourceRef != "" {
		sourceCommit := extractSourceCommit(sourceRef)
		if sourceCommit != "" {
			content, _, err := start.GetSessionLog(sourceCommit)
			if err != nil {
				return fmt.Errorf("failed to get session log: %w", err)
			}
			fmt.Print(string(content))
			return nil
		}
	}

	return fmt.Errorf("commit %s has no session metadata", commitRef)
}

// parseSessionTrailers extracts Entire-Session and Entire-Source-Ref from commit message
func parseSessionTrailers(message string) (sessionID, sourceRef string) {
	sessionRe := regexp.MustCompile(paths.SessionTrailerKey + `:\s*(.+)`)
	sourceRe := regexp.MustCompile(paths.SourceRefTrailerKey + `:\s*(.+)`)

	if matches := sessionRe.FindStringSubmatch(message); len(matches) > 1 {
		sessionID = strings.TrimSpace(matches[1])
	}
	if matches := sourceRe.FindStringSubmatch(message); len(matches) > 1 {
		sourceRef = strings.TrimSpace(matches[1])
	}

	return sessionID, sourceRef
}

// extractSourceCommit extracts the commit hash from a source ref (e.g., "entire/abc1234@def567890123")
func extractSourceCommit(sourceRef string) string {
	parts := strings.Split(sourceRef, "@")
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}

func newSessionListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
		Long:  "List all sessions stored by the current strategy",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSessionList()
		},
	}
	return cmd
}

func newSessionResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume [session-id]",
		Short: "Resume a session and restore agent memory",
		Long: `Resume a session by setting it as current and restoring the agent's memory.

The session ID can be a prefix - the first matching session will be used.

If no session ID is provided, an interactive picker will be shown with available
sessions from the current branch.`,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runSessionResumeInteractive()
			}
			return runSessionResume(args[0])
		},
	}

	return cmd
}

func newSessionCurrentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "current",
		Short: "Show current session details",
		Long:  "Show details of the current session from .entire/current_session",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSessionCurrent()
		},
	}
	return cmd
}

func runSessionList() error {
	sessions, err := strategy.ListSessions()
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	// Sort sessions by most recent activity (descending)
	// Falls back to session ID (which contains date) when timestamps are equal
	sort.Slice(sessions, func(i, j int) bool {
		// Get most recent timestamp for each session
		iTime := sessions[i].StartTime
		for _, cp := range sessions[i].Checkpoints {
			if cp.Timestamp.After(iTime) {
				iTime = cp.Timestamp
			}
		}
		jTime := sessions[j].StartTime
		for _, cp := range sessions[j].Checkpoints {
			if cp.Timestamp.After(jTime) {
				jTime = cp.Timestamp
			}
		}
		// If timestamps are equal, sort by session ID descending (newer dates first)
		if iTime.Equal(jTime) {
			return sessions[i].ID > sessions[j].ID
		}
		return iTime.After(jTime) // Most recent first
	})

	// Get current session ID for marking (ignore error, empty string is fine)
	currentSessionID, readErr := paths.ReadCurrentSession()
	if readErr != nil {
		currentSessionID = ""
	}

	// Print header (2-space indent to align with marker column)
	fmt.Printf("  %-19s  %-11s  %s\n", "session-id", "Checkpoints", "Description")
	fmt.Printf("  %-19s  %-11s  %s\n", "───────────────────", "───────────", "────────────────────────────────────────────────────────────────")

	for _, sess := range sessions {
		// Show session ID - truncate to 19 chars for display
		// This preserves enough for prefix matching (e.g., "2025-12-01-8f76b0e8")
		displayID := sess.ID
		if len(displayID) > 19 {
			displayID = displayID[:19]
		}

		// Mark current session
		marker := "  "
		if currentSessionID != "" && (sess.ID == currentSessionID || strings.HasPrefix(sess.ID, currentSessionID)) {
			marker = "* "
		}

		// Checkpoint count
		checkpoints := len(sess.Checkpoints)

		// Truncate description for display
		description := sess.Description
		if len(description) > 64 {
			description = description[:61] + "..."
		}

		fmt.Printf("%s%-19s  %-11d  %s\n", marker, displayID, checkpoints, description)
	}

	// Print usage hint
	fmt.Println()
	fmt.Println("Resume a session: entire session resume <session-id>")

	return nil
}

func runSessionResume(sessionID string) error {
	// Get the agent for resume command formatting
	ag, err := agent.Detect()
	if err != nil {
		ag = agent.Default()
	}

	strat := GetStrategy()

	// Verify session exists
	session, err := strategy.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, strategy.ErrNoSession) {
			return fmt.Errorf("session not found: %s", sessionID)
		}
		return fmt.Errorf("failed to find session: %w", err)
	}

	// Check if session is already active (ignore error - empty string is fine if no current session)
	currentSessionID, _ := paths.ReadCurrentSession() //nolint:errcheck // Empty string is acceptable if no current session
	if currentSessionID == session.ID {
		fmt.Fprintf(os.Stderr, "Session already active: %s\n", session.ID)
		agentSessionID := ag.ExtractAgentSessionID(session.ID)
		fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
		fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSessionID))
		return nil
	}

	// Write the full session ID to current_session file
	if err := paths.WriteCurrentSession(session.ID); err != nil {
		return fmt.Errorf("failed to set current session: %w", err)
	}

	// Find the most recent checkpoint for this session to get the session log
	var checkpointID string
	if len(session.Checkpoints) > 0 {
		// Get the most recent checkpoint
		checkpointID = session.Checkpoints[len(session.Checkpoints)-1].CheckpointID
	}

	// Restore agent session if we have a checkpoint
	if checkpointID != "" {
		if err := restoreAgentSession(ag, session.ID, checkpointID, strat); err != nil {
			// Non-fatal: session is set, but memory restoration failed
			fmt.Fprintf(os.Stderr, "Warning: could not restore session: %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr, "Session resumed: %s\n", session.ID)
	agentSessionID := ag.ExtractAgentSessionID(session.ID)
	fmt.Fprintf(os.Stderr, "\nTo continue this session, run:\n")
	fmt.Fprintf(os.Stderr, "  %s\n", ag.FormatResumeCommand(agentSessionID))

	return nil
}

// restoreAgentSession restores the session transcript using the agent abstraction.
// This is agent-agnostic and works with any registered agent.
func restoreAgentSession(ag agent.Agent, sessionID, checkpointID string, strat strategy.Strategy) error {
	// Get repo root for session directory lookup
	// Use repo root instead of CWD because Claude stores sessions per-repo,
	// and running from a subdirectory would look up the wrong session directory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	sessionDir, err := ag.GetSessionDir(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to determine session directory: %w", err)
	}

	// Extract agent-specific session ID from Entire session ID
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	sessionLogPath := filepath.Join(sessionDir, agentSessionID+".jsonl")

	// Check if session log already exists
	if fileExists(sessionLogPath) {
		return nil // Already restored
	}

	// Get session log content
	logContent, _, err := strat.GetSessionLog(checkpointID)
	if err != nil {
		if errors.Is(err, strategy.ErrNoMetadata) {
			return nil // No metadata available, skip restoration
		}
		return fmt.Errorf("failed to get session log: %w", err)
	}

	// Create an AgentSession with the native data
	agentSession := &agent.AgentSession{
		SessionID:  agentSessionID,
		AgentName:  ag.Name(),
		RepoPath:   repoRoot,
		SessionRef: sessionLogPath,
		NativeData: logContent,
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Write the session using the agent's WriteSession method
	if err := ag.WriteSession(agentSession); err != nil {
		return fmt.Errorf("failed to write session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Session restored to: %s\n", sessionLogPath)
	return nil
}

// runSessionResumeInteractive shows an interactive picker for sessions.
func runSessionResumeInteractive() error {
	sessions, err := strategy.ListSessions()
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(sessions) == 0 {
		return errors.New("no sessions found")
	}

	// Filter sessions to only those relevant to the current branch
	sessions = filterSessionsForCurrentBranch(sessions)

	if len(sessions) == 0 {
		return errors.New("no sessions found for current branch")
	}

	// Sort sessions by most recent activity
	sort.Slice(sessions, func(i, j int) bool {
		iTime := sessions[i].StartTime
		for _, cp := range sessions[i].Checkpoints {
			if cp.Timestamp.After(iTime) {
				iTime = cp.Timestamp
			}
		}
		jTime := sessions[j].StartTime
		for _, cp := range sessions[j].Checkpoints {
			if cp.Timestamp.After(jTime) {
				jTime = cp.Timestamp
			}
		}
		if iTime.Equal(jTime) {
			return sessions[i].ID > sessions[j].ID
		}
		return iTime.After(jTime)
	})

	// Build options for the picker
	options := buildSessionOptions(sessions)

	var selectedID string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select a session to resume").
				Description("Choose a session to set as current and restore agent memory").
				Options(options...).
				Value(&selectedID),
		),
	)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return fmt.Errorf("selection cancelled: %w", err)
		}
		return fmt.Errorf("failed to get selection: %w", err)
	}

	if selectedID == sessionPickerCancelValue {
		// User selected cancel
		fmt.Println("Session resume cancelled.")
		return nil
	}

	return runSessionResume(selectedID)
}

// filterSessionsForCurrentBranch filters sessions to only those linked to the current branch.
// Finds Entire-Checkpoint trailers in branch commits.
func filterSessionsForCurrentBranch(sessions []strategy.Session) []strategy.Session {
	repo, err := openRepository()
	if err != nil {
		return sessions
	}

	head, err := repo.Head()
	if err != nil {
		return sessions
	}

	headHash := head.Hash()
	matchingSessionIDs := make(map[string]bool)

	// Find Entire-Checkpoint trailers in branch commits
	checkpointToSession := make(map[string]string)
	for _, sess := range sessions {
		for _, cp := range sess.Checkpoints {
			if cp.CheckpointID != "" {
				checkpointToSession[cp.CheckpointID] = sess.ID
			}
		}
	}

	iter, err := repo.Log(&git.LogOptions{From: headHash})
	if err != nil {
		return sessions
	}
	// Get the main branch commit hash to determine branch-only commits
	mainBranchHash := strategy.GetMainBranchHash(repo)

	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search
		count++
		if count > maxCommitsToScanForCheckpointsUntilMainBranch {
			return errors.New("limit reached")
		}
		if mainBranchHash != plumbing.ZeroHash && c.Hash.String() == mainBranchHash.String() {
			return nil
		}

		if checkpointID, found := paths.ParseCheckpointTrailer(c.Message); found {
			if sessionID, ok := checkpointToSession[checkpointID]; ok {
				matchingSessionIDs[sessionID] = true
			}
		}
		return nil
	})

	// Filter sessions
	filtered := make([]strategy.Session, 0, len(sessions))
	for _, sess := range sessions {
		if matchingSessionIDs[sess.ID] {
			filtered = append(filtered, sess)
		}
	}

	return filtered
}

// buildSessionOptions creates huh.Option items from sessions for the picker.
func buildSessionOptions(sessions []strategy.Session) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(sessions)+1)

	for _, sess := range sessions {
		// Format: session-id (N checkpoints) - description
		displayID := sess.ID
		if len(displayID) > 19 {
			displayID = displayID[:19]
		}

		label := displayID
		if len(sess.Checkpoints) > 0 {
			label = fmt.Sprintf("%s (%d checkpoints)", displayID, len(sess.Checkpoints))
		}
		if sess.Description != "" && sess.Description != strategy.NoDescription {
			desc := sess.Description
			if len(desc) > 40 {
				desc = desc[:37] + "..."
			}
			label = fmt.Sprintf("%s - %s", label, desc)
		}

		options = append(options, huh.NewOption(label, sess.ID))
	}

	// Add cancel option
	options = append(options, huh.NewOption("Cancel", sessionPickerCancelValue))

	return options
}

func runSessionCurrent() error {
	currentSessionID, err := paths.ReadCurrentSession()
	if err != nil {
		return fmt.Errorf("failed to read current session: %w", err)
	}

	if currentSessionID == "" {
		fmt.Println("No current session set.")
		return nil
	}

	strat := GetStrategy()

	session, sessErr := strategy.GetSession(currentSessionID)
	if sessErr != nil {
		// Session ID is set but not found in strategy
		fmt.Printf("Session: %s (not found in %s strategy)\n", currentSessionID, strat.Name())
		return nil //nolint:nilerr // Display session ID even when not found in strategy
	}

	fmt.Printf("Session:     %s\n", session.ID)
	fmt.Printf("Strategy:    %s\n", session.Strategy)
	if session.Description != "" && session.Description != strategy.NoDescription {
		fmt.Printf("Description: %s\n", session.Description)
	}
	if !session.StartTime.IsZero() {
		fmt.Printf("Started:     %s\n", session.StartTime.Format("2006-01-02 15:04"))
	}
	if len(session.Checkpoints) > 0 {
		fmt.Printf("Checkpoints: %d\n", len(session.Checkpoints))
	}

	return nil
}
