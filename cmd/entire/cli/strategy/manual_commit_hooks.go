package strategy

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/stringutil"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// askConfirmTTY prompts the user for a yes/no confirmation via /dev/tty.
// This works even when stdin is redirected (e.g., git commit -m).
// Returns true for yes, false for no. If TTY is unavailable, returns the default.
// If context is non-empty, it is displayed on a separate line before the prompt.
func askConfirmTTY(prompt string, context string, defaultYes bool) bool {
	// Open /dev/tty for both reading and writing
	// This is the controlling terminal, which works even when stdin/stderr are redirected
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// Can't open TTY (e.g., running in CI), use default
		return defaultYes
	}
	defer tty.Close()

	// Show context if provided
	if context != "" {
		fmt.Fprintf(tty, "%s\n", context)
	}

	// Show prompt with default indicator
	// Write to tty directly, not stderr, since git hooks may redirect stderr to /dev/null
	var hint string
	if defaultYes {
		hint = "[Y/n]"
	} else {
		hint = "[y/N]"
	}
	fmt.Fprintf(tty, "%s %s ", prompt, hint)

	// Read response
	reader := bufio.NewReader(tty)
	response, err := reader.ReadString('\n')
	if err != nil {
		return defaultYes
	}

	response = strings.TrimSpace(strings.ToLower(response))
	switch response {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		// Empty or invalid input - use default
		return defaultYes
	}
}

// CommitMsg is called by the git commit-msg hook after the user edits the message.
// If the message contains only our trailer (no actual user content), strip it
// so git will abort the commit due to empty message.
//
//nolint:unparam // error return required by interface but hooks must return nil
func (s *ManualCommitStrategy) CommitMsg(commitMsgFile string) error {
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // Path comes from git hook
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	message := string(content)

	// Check if our trailer is present
	checkpointID, found := paths.ParseCheckpointTrailer(message)
	if !found || checkpointID == "" {
		// No trailer, nothing to do
		return nil
	}

	// Check if there's any user content (non-comment, non-trailer lines)
	if !hasUserContent(message) {
		// No user content - strip the trailer so git aborts
		message = stripCheckpointTrailer(message)
		if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil {
			return nil //nolint:nilerr // Hook must be silent on failure
		}
	}

	return nil
}

// hasUserContent checks if the message has any content besides comments and our trailer.
func hasUserContent(message string) bool {
	trailerPrefix := paths.CheckpointTrailerKey + ":"
	for _, line := range strings.Split(message, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip empty lines
		if trimmed == "" {
			continue
		}
		// Skip comment lines
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Skip our trailer line
		if strings.HasPrefix(trimmed, trailerPrefix) {
			continue
		}
		// Found user content
		return true
	}
	return false
}

// stripCheckpointTrailer removes the Entire-Checkpoint trailer line from the message.
func stripCheckpointTrailer(message string) string {
	trailerPrefix := paths.CheckpointTrailerKey + ":"
	var result []string
	for _, line := range strings.Split(message, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), trailerPrefix) {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// PrepareCommitMsg is called by the git prepare-commit-msg hook.
// Adds an Entire-Checkpoint trailer to the commit message with a stable checkpoint ID.
// Only adds a trailer if there's actually new session content to condense.
// The actual condensation happens in PostCommit - if the user removes the trailer,
// the commit will not be linked to the session (useful for "manual" commits).
// For amended commits, preserves the existing checkpoint ID.
//
// The source parameter indicates how the commit was initiated:
//   - "" or "template": normal editor flow - adds trailer with explanatory comment
//   - "message": using -m or -F flag - prompts user interactively via /dev/tty
//   - "merge", "squash", "commit": skip trailer entirely (auto-generated or amend commits)
//
//nolint:unparam // error return required by interface but hooks must return nil
func (s *ManualCommitStrategy) PrepareCommitMsg(commitMsgFile string, source string) error {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	// Skip for merge, squash, and commit (amend) sources
	// These are auto-generated or reusing existing messages - not from Claude sessions
	switch source {
	case "merge", "squash", "commit":
		logging.Debug(logCtx, "prepare-commit-msg: skipped for source",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
		)
		return nil
	}
	repo, err := OpenRepository()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	worktreePath, err := GetWorktreePath()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	// Find all active sessions for this worktree
	// We match by worktree (not BaseCommit) because the user may have made
	// intermediate commits without entering new prompts, causing HEAD to diverge
	sessions, err := s.findSessionsForWorktree(worktreePath)
	if err != nil || len(sessions) == 0 {
		// No active sessions or error listing - silently skip (hooks must be resilient)
		logging.Debug(logCtx, "prepare-commit-msg: no active sessions",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
		)
		return nil //nolint:nilerr // Intentional: hooks must be silent on failure
	}

	// Check if any session has new content to condense
	sessionsWithContent := s.filterSessionsWithNewContent(repo, sessions)

	// Determine which checkpoint ID to use
	var checkpointID string
	var hasNewContent bool
	var reusedSession *SessionState

	if len(sessionsWithContent) > 0 {
		// New content exists - will generate new checkpoint ID below
		hasNewContent = true
	} else {
		// No new content - check if any session has a LastCheckpointID to reuse
		// This handles the case where user splits Claude's work across multiple commits
		// Reuse if: LastCheckpointID exists AND (FilesTouched is empty OR files overlap)
		// - FilesTouched empty: commits made before session stop, reuse checkpoint
		// - FilesTouched populated: only reuse if files overlap (prevents unrelated commits from reusing)

		// Get current HEAD to filter sessions
		head, err := repo.Head()
		if err != nil {
			return nil //nolint:nilerr // Hook must be silent on failure
		}
		currentHeadHash := head.Hash().String()

		// Filter to sessions where BaseCommit matches current HEAD
		// This prevents reusing checkpoint IDs from old sessions
		var currentSessions []*SessionState
		for _, session := range sessions {
			if session.BaseCommit == currentHeadHash {
				currentSessions = append(currentSessions, session)
			}
		}

		// If no sessions match current HEAD, fall back to all sessions
		// (for backward compatibility with sessions created before BaseCommit tracking)
		if len(currentSessions) == 0 {
			currentSessions = sessions
		}

		stagedFiles := getStagedFiles(repo)
		for _, session := range currentSessions {
			if session.LastCheckpointID != "" &&
				(len(session.FilesTouched) == 0 || hasOverlappingFiles(stagedFiles, session.FilesTouched)) {
				checkpointID = session.LastCheckpointID
				reusedSession = session
				break
			}
		}
		if checkpointID == "" {
			// No new content and no previous checkpoint to reference (or staged files are unrelated)
			logging.Debug(logCtx, "prepare-commit-msg: no content to link",
				slog.String("strategy", "manual-commit"),
				slog.String("source", source),
				slog.Int("sessions_found", len(sessions)),
				slog.Int("sessions_with_content", len(sessionsWithContent)),
			)
			return nil
		}
	}

	// Read current commit message
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	message := string(content)

	// Get or generate checkpoint ID
	existingID, found := paths.ParseCheckpointTrailer(message)
	if found && existingID != "" {
		// Trailer already exists (e.g., amend) - keep it
		logging.Debug(logCtx, "prepare-commit-msg: trailer already exists",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
			slog.String("existing_checkpoint_id", existingID),
		)
		return nil
	}

	if hasNewContent {
		// New content: generate new checkpoint ID
		checkpointID = paths.GenerateCheckpointID()
	}
	// Otherwise checkpointID is already set to LastCheckpointID from above

	// Determine agent name and last prompt from session
	agentName := DefaultAgentType // default for backward compatibility
	var lastPrompt string
	if hasNewContent && len(sessionsWithContent) > 0 {
		session := sessionsWithContent[0]
		if session.AgentType != "" {
			agentName = session.AgentType
		}
		lastPrompt = s.getLastPrompt(repo, session)
	} else if reusedSession != nil {
		// Reusing checkpoint from existing session - get agent type and prompt from that session
		if reusedSession.AgentType != "" {
			agentName = reusedSession.AgentType
		}
		lastPrompt = s.getLastPrompt(repo, reusedSession)
	}

	// Prepare prompt for display: collapse newlines/whitespace, then truncate (rune-safe)
	displayPrompt := stringutil.TruncateRunes(stringutil.CollapseWhitespace(lastPrompt), 80, "...")

	// Add trailer differently based on commit source
	if source == "message" {
		// Using -m or -F: ask user interactively whether to add trailer
		// (comments won't be stripped by git in this mode)

		// Build context string for interactive prompt
		var promptContext string
		if displayPrompt != "" {
			promptContext = "You have an active " + agentName + " session.\nLast Prompt: " + displayPrompt
		}

		if !askConfirmTTY("Link this commit to "+agentName+" session context?", promptContext, true) {
			// User declined - don't add trailer
			logging.Debug(logCtx, "prepare-commit-msg: user declined trailer",
				slog.String("strategy", "manual-commit"),
				slog.String("source", source),
			)
			return nil
		}
		message = addCheckpointTrailer(message, checkpointID)
	} else {
		// Normal editor flow: add trailer with explanatory comment (will be stripped by git)
		message = addCheckpointTrailerWithComment(message, checkpointID, agentName, displayPrompt)
	}

	logging.Info(logCtx, "prepare-commit-msg: trailer added",
		slog.String("strategy", "manual-commit"),
		slog.String("source", source),
		slog.String("checkpoint_id", checkpointID),
		slog.Bool("has_new_content", hasNewContent),
	)

	// Write updated message back
	if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	return nil
}

// PostCommit is called by the git post-commit hook after a commit is created.
// Checks if the commit has an Entire-Checkpoint trailer and if so, condenses
// session data from shadow branches to entire/sessions.
// If the user removed the trailer during commit message editing, this is treated
// as a "manual" commit and no condensation happens.
//
//nolint:unparam // error return required by interface but hooks must return nil
func (s *ManualCommitStrategy) PostCommit() error {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	repo, err := OpenRepository()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	// Get HEAD commit to check for trailer
	head, err := repo.Head()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	// Check if commit has checkpoint trailer
	checkpointID, found := paths.ParseCheckpointTrailer(commit.Message)
	if !found || checkpointID == "" {
		// No trailer - user removed it, treat as manual commit
		logging.Debug(logCtx, "post-commit: no checkpoint trailer",
			slog.String("strategy", "manual-commit"),
		)
		return nil
	}

	worktreePath, err := GetWorktreePath()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	// Find all active sessions for this worktree
	sessions, err := s.findSessionsForWorktree(worktreePath)
	if err != nil || len(sessions) == 0 {
		logging.Warn(logCtx, "post-commit: no active sessions despite trailer",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_id", checkpointID),
		)
		return nil //nolint:nilerr // Intentional: hooks must be silent on failure
	}

	// Filter to sessions with new content
	sessionsWithContent := s.filterSessionsWithNewContent(repo, sessions)
	if len(sessionsWithContent) == 0 {
		logging.Warn(logCtx, "post-commit: no new content to condense",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_id", checkpointID),
			slog.Int("sessions_found", len(sessions)),
		)
		return nil
	}

	// Track shadow branches to clean up after successful condensation
	shadowBranchesToDelete := make(map[string]struct{})

	// Condense sessions that have new content
	for _, state := range sessionsWithContent {
		result, err := s.CondenseSession(repo, checkpointID, state)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[entire] Warning: condensation failed for session %s: %v\n",
				state.SessionID, err)
			continue
		}

		// Track this shadow branch for cleanup
		shadowBranchesToDelete[state.BaseCommit] = struct{}{}

		// Update session state for the new base commit
		// After condensation, the session continues from the NEW commit (HEAD), so we:
		// 1. Update BaseCommit to new HEAD - session now tracks from new commit
		// 2. Reset CheckpointCount to 0 - no checkpoints exist on new shadow branch yet
		// 3. Update CondensedTranscriptLines - track transcript offset for incremental context
		//
		// This is critical: if we don't update BaseCommit, listAllSessionStates will try
		// to find shadow branch for old commit (which gets deleted), and since CheckpointCount > 0,
		// it will clean up (delete) the session state file. By updating to new HEAD with
		// CheckpointCount = 0, the session is preserved even without a shadow branch.
		state.BaseCommit = head.Hash().String()
		state.CheckpointCount = 0
		state.CondensedTranscriptLines = result.TotalTranscriptLines

		// Save checkpoint ID so subsequent commits without new content can reuse it
		state.LastCheckpointID = checkpointID

		if err := s.saveSessionState(state); err != nil {
			fmt.Fprintf(os.Stderr, "[entire] Warning: failed to update session state: %v\n", err)
		}

		shortID := state.SessionID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		fmt.Fprintf(os.Stderr, "[entire] Condensed session %s: %s (%d steps)\n",
			shortID, result.CheckpointID, result.StepsCount)

		// Log condensation
		logCtx := logging.WithComponent(context.Background(), "checkpoint")
		logging.Info(logCtx, "session condensed",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_id", result.CheckpointID),
			slog.Int("steps_condensed", result.StepsCount),
			slog.Int("transcript_lines", result.TotalTranscriptLines),
		)
	}

	// Clean up shadow branches after successful condensation
	// Data is now preserved on entire/sessions, so shadow branches are no longer needed
	for baseCommit := range shadowBranchesToDelete {
		shadowBranchName := getShadowBranchNameForCommit(baseCommit)
		if err := deleteShadowBranch(repo, shadowBranchName); err != nil {
			fmt.Fprintf(os.Stderr, "[entire] Warning: failed to clean up %s: %v\n", shadowBranchName, err)
		} else {
			fmt.Fprintf(os.Stderr, "[entire] Cleaned up shadow branch: %s\n", shadowBranchName)

			// Log branch cleanup
			logCtx := logging.WithComponent(context.Background(), "checkpoint")
			logging.Info(logCtx, "shadow branch deleted",
				slog.String("strategy", "manual-commit"),
				slog.String("shadow_branch", shadowBranchName),
			)
		}
	}

	return nil
}

// filterSessionsWithNewContent returns sessions that have new transcript content
// beyond what was already condensed.
func (s *ManualCommitStrategy) filterSessionsWithNewContent(repo *git.Repository, sessions []*SessionState) []*SessionState {
	var result []*SessionState

	for _, state := range sessions {
		hasNew, err := s.sessionHasNewContent(repo, state)
		if err != nil {
			// On error, include the session (fail open for hooks)
			result = append(result, state)
			continue
		}
		if hasNew {
			result = append(result, state)
		}
	}

	return result
}

// sessionHasNewContent checks if a session has new transcript content
// beyond what was already condensed.
func (s *ManualCommitStrategy) sessionHasNewContent(repo *git.Repository, state *SessionState) (bool, error) {
	// Get shadow branch
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		// No shadow branch means no new checkpoints have been created since
		// the last condensation. This is expected after condensation when
		// BaseCommit was updated to the new HEAD.
		return false, nil //nolint:nilerr // Expected: no shadow branch = no new content
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return false, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("failed to get commit tree: %w", err)
	}

	// Look for transcript file
	metadataDir := paths.EntireMetadataDir + "/" + state.SessionID
	var transcriptLines int

	if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileName); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			transcriptLines = countTranscriptLines(content)
		}
	} else if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileNameLegacy); fileErr == nil {
		if content, contentErr := file.Contents(); contentErr == nil {
			transcriptLines = countTranscriptLines(content)
		}
	}

	// Has new content if there are more lines than already condensed
	return transcriptLines > state.CondensedTranscriptLines, nil
}

// countTranscriptLines counts lines in a transcript, matching the counting method used
// in extractSessionData for consistency. This trims trailing empty lines (from final \n
// in JSONL) but includes empty lines in the middle of the file.
func countTranscriptLines(content string) int {
	lines := strings.Split(content, "\n")
	// Trim trailing empty lines (from final \n in JSONL)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return len(lines)
}

// addCheckpointTrailer adds the Entire-Checkpoint trailer to a commit message.
// Handles proper trailer formatting (blank line before trailers if needed).
func addCheckpointTrailer(message, checkpointID string) string {
	trailer := paths.CheckpointTrailerKey + ": " + checkpointID

	// If message already ends with trailers (lines starting with key:), just append
	// Otherwise, add a blank line first
	lines := strings.Split(strings.TrimRight(message, "\n"), "\n")

	// Check if last non-empty, non-comment line looks like a trailer
	// Git comment lines start with # and may contain ": " (e.g., "# Changes to be committed:")
	hasTrailers := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			break
		}
		// Skip git comment lines
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, ": ") {
			hasTrailers = true
			break
		}
		// Non-comment, non-trailer line found - no existing trailers
		break
	}

	if hasTrailers {
		// Append trailer directly
		return strings.TrimRight(message, "\n") + "\n" + trailer + "\n"
	}

	// Add blank line before trailer
	return strings.TrimRight(message, "\n") + "\n\n" + trailer + "\n"
}

// addCheckpointTrailerWithComment adds the Entire-Checkpoint trailer with an explanatory comment.
// The trailer is placed above the git comment block but below the user's message area,
// with a comment explaining that the user can remove it if they don't want to link the commit
// to the agent session. If prompt is non-empty, it's shown as context.
func addCheckpointTrailerWithComment(message, checkpointID, agentName, prompt string) string {
	trailer := paths.CheckpointTrailerKey + ": " + checkpointID
	commentLines := []string{
		"# Remove the Entire-Checkpoint trailer above if you don't want to link this commit to " + agentName + " session context.",
	}
	if prompt != "" {
		commentLines = append(commentLines, "# Last Prompt: "+prompt)
	}
	commentLines = append(commentLines, "# The trailer will be added to your next commit based on this branch.")
	comment := strings.Join(commentLines, "\n")

	lines := strings.Split(message, "\n")

	// Find where the git comment block starts (first # line)
	commentStart := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			commentStart = i
			break
		}
	}

	if commentStart == -1 {
		// No git comments, append trailer at the end
		return strings.TrimRight(message, "\n") + "\n\n" + trailer + "\n" + comment + "\n"
	}

	// Split into user content and git comments
	userContent := strings.Join(lines[:commentStart], "\n")
	gitComments := strings.Join(lines[commentStart:], "\n")

	// Build result: user content, blank line, trailer, comment, blank line, git comments
	userContent = strings.TrimRight(userContent, "\n")
	if userContent == "" {
		// No user content yet - leave space for them to type, then trailer
		// Two newlines: first for user's message line, second for blank separator
		return "\n\n" + trailer + "\n" + comment + "\n\n" + gitComments
	}
	return userContent + "\n\n" + trailer + "\n" + comment + "\n\n" + gitComments
}

// InitializeSession creates session state for a new session or updates an existing one.
// This implements the optional SessionInitializer interface.
// Called during UserPromptSubmit to allow git hooks to detect active sessions.
//
// If the session already exists and HEAD has moved (e.g., user committed), updates
// BaseCommit to the new HEAD so future checkpoints go to the correct shadow branch.
//
// If there's an existing shadow branch with activity from a different worktree,
// returns a ShadowBranchConflictError to allow the caller to inform the user.
//
// agentType is the human-readable name of the agent (e.g., "Claude Code").
func (s *ManualCommitStrategy) InitializeSession(sessionID string, agentType string) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get current HEAD
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Check if session already exists
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to check session state: %w", err)
	}

	// Check for shadow branch conflict before proceeding
	// This must happen even if session state exists but has no checkpoints yet
	// (e.g., state was created by concurrent warning but conflict later resolved)
	baseCommitHash := head.Hash().String()
	if state == nil || state.CheckpointCount == 0 {
		shadowBranch := getShadowBranchNameForCommit(baseCommitHash)
		refName := plumbing.NewBranchReferenceName(shadowBranch)

		ref, refErr := repo.Reference(refName, true)
		if refErr == nil {
			// Shadow branch exists - check if it has commits from a different session
			tipCommit, commitErr := repo.CommitObject(ref.Hash())
			if commitErr == nil {
				existingSessionID, found := paths.ParseSessionTrailer(tipCommit.Message)
				if found && existingSessionID != sessionID {
					// Check if the existing session has a state file
					// existingSessionID is the full Entire session ID (YYYY-MM-DD-uuid) from the trailer
					// We intentionally ignore load errors - treat them as "no state" (orphaned branch)
					existingState, _ := s.loadSessionState(existingSessionID) //nolint:errcheck // error means no state
					if existingState == nil {
						// Orphaned shadow branch - no state file for the existing session
						// Reset the branch so the new session can proceed
						fmt.Fprintf(os.Stderr, "Resetting orphaned shadow branch '%s' (previous session %s has no state)\n",
							shadowBranch, existingSessionID)
						if err := deleteShadowBranch(repo, shadowBranch); err != nil {
							return fmt.Errorf("failed to reset orphaned shadow branch: %w", err)
						}
					} else {
						// Existing session has state - this is a real conflict
						// (e.g., different worktree at same commit)
						return &SessionIDConflictError{
							ExistingSession: existingSessionID,
							NewSession:      sessionID,
							ShadowBranch:    shadowBranch,
						}
					}
				}
			}
		}
	}

	if state != nil && state.BaseCommit != "" {
		// Session is fully initialized
		needSave := false

		// Backfill AgentType if empty (for sessions created before the agent_type field was added)
		if state.AgentType == "" && agentType != "" {
			state.AgentType = agentType
			needSave = true
		}

		// Clear LastCheckpointID on every new prompt
		// This is set during PostCommit when a checkpoint is created, and should be
		// cleared when the user enters a new prompt (starting fresh work)
		if state.LastCheckpointID != "" {
			state.LastCheckpointID = ""
			needSave = true
		}

		// Check if HEAD has moved (user pulled/rebased or committed)
		if state.BaseCommit != head.Hash().String() {
			oldBaseCommit := state.BaseCommit
			newBaseCommit := head.Hash().String()

			// Check if old shadow branch exists - if so, user did NOT commit (would have been deleted)
			// This happens when user does: stash → pull → stash apply, or rebase, etc.
			oldShadowBranch := getShadowBranchNameForCommit(oldBaseCommit)
			oldRefName := plumbing.NewBranchReferenceName(oldShadowBranch)
			if oldRef, err := repo.Reference(oldRefName, true); err == nil {
				// Old shadow branch exists - move it to new base commit
				newShadowBranch := getShadowBranchNameForCommit(newBaseCommit)
				newRefName := plumbing.NewBranchReferenceName(newShadowBranch)

				// Create new reference pointing to same commit
				newRef := plumbing.NewHashReference(newRefName, oldRef.Hash())
				if err := repo.Storer.SetReference(newRef); err != nil {
					return fmt.Errorf("failed to create new shadow branch %s: %w", newShadowBranch, err)
				}

				// Delete old reference
				if err := repo.Storer.RemoveReference(oldRefName); err != nil {
					// Non-fatal: log but continue
					fmt.Fprintf(os.Stderr, "Warning: failed to remove old shadow branch %s: %v\n", oldShadowBranch, err)
				}

				fmt.Fprintf(os.Stderr, "Moved shadow branch from %s to %s (base commit changed after pull/rebase)\n",
					oldShadowBranch, newShadowBranch)
			}

			state.BaseCommit = newBaseCommit
			needSave = true
			fmt.Fprintf(os.Stderr, "Updated session base commit to %s\n", newBaseCommit[:7])
		}

		if needSave {
			if err := s.saveSessionState(state); err != nil {
				return fmt.Errorf("failed to update session state: %w", err)
			}
		}
		return nil
	}
	// If state exists but BaseCommit is empty, it's a partial state from concurrent warning
	// Continue below to properly initialize it

	currentWorktree, err := GetWorktreePath()
	if err != nil {
		return fmt.Errorf("failed to get worktree path: %w", err)
	}

	// Check for existing sessions on the same base commit from different worktrees
	existingSessions, err := s.findSessionsForCommit(head.Hash().String())
	if err != nil {
		// Log but continue - conflict detection is best-effort
		fmt.Fprintf(os.Stderr, "Warning: failed to check for existing sessions: %v\n", err)
	} else {
		for _, existingState := range existingSessions {
			// Skip sessions from the same worktree
			if existingState.WorktreePath == currentWorktree {
				continue
			}

			// Found a session from a different worktree on the same base commit
			shadowBranch := getShadowBranchNameForCommit(head.Hash().String())
			return &ShadowBranchConflictError{
				Branch:           shadowBranch,
				ExistingSession:  existingState.SessionID,
				ExistingWorktree: existingState.WorktreePath,
				LastActivity:     existingState.StartedAt,
				CurrentSession:   sessionID,
				CurrentWorktree:  currentWorktree,
			}
		}
	}

	// Initialize new session
	_, err = s.initializeSession(repo, sessionID, agentType)
	if err != nil {
		return fmt.Errorf("failed to initialize session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Initialized shadow session: %s\n", sessionID)
	return nil
}

// getStagedFiles returns a list of files staged for commit.
func getStagedFiles(repo *git.Repository) []string {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil
	}

	status, err := worktree.Status()
	if err != nil {
		return nil
	}

	var staged []string
	for path, fileStatus := range status {
		// Check if file is staged (in index)
		if fileStatus.Staging != git.Unmodified && fileStatus.Staging != git.Untracked {
			staged = append(staged, path)
		}
	}
	return staged
}

// getLastPrompt retrieves the most recent user prompt from a session's shadow branch.
// Returns empty string if no prompt can be retrieved.
func (s *ManualCommitStrategy) getLastPrompt(repo *git.Repository, state *SessionState) string {
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	// Extract session data (using 0 as startLine to get all prompts)
	sessionData, err := s.extractSessionData(repo, ref.Hash(), state.SessionID, 0, nil)
	if err != nil || len(sessionData.Prompts) == 0 {
		return ""
	}

	// Return the last prompt (most recent work before commit)
	return sessionData.Prompts[len(sessionData.Prompts)-1]
}

// hasOverlappingFiles checks if any file in stagedFiles appears in filesTouched.
func hasOverlappingFiles(stagedFiles, filesTouched []string) bool {
	touchedSet := make(map[string]bool)
	for _, f := range filesTouched {
		touchedSet[f] = true
	}

	for _, staged := range stagedFiles {
		if touchedSet[staged] {
			return true
		}
	}
	return false
}
