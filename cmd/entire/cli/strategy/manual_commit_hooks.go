package strategy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/binary"
)

// hasTTY checks if /dev/tty is available for interactive prompts.
// Returns false when running as an agent subprocess (no controlling terminal).
//
// In test environments, ENTIRE_TEST_TTY overrides the real check:
//   - ENTIRE_TEST_TTY=1 → simulate human (TTY available)
//   - ENTIRE_TEST_TTY=0 → simulate agent (no TTY)
func hasTTY() bool {
	if v := os.Getenv("ENTIRE_TEST_TTY"); v != "" {
		return v == "1"
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// askConfirmTTY prompts the user for a yes/no confirmation via /dev/tty.
// This works even when stdin is redirected (e.g., git commit -m).
// Returns true for yes, false for no. If TTY is unavailable, returns the default.
// If context is non-empty, it is displayed on a separate line before the prompt.
func askConfirmTTY(prompt string, context string, defaultYes bool) bool {
	// In test mode, don't try to interact with the real TTY — just use the default.
	// ENTIRE_TEST_TTY=1 simulates "a human is present" for the hasTTY() check
	// but we can't actually read from the TTY in tests.
	if os.Getenv("ENTIRE_TEST_TTY") != "" {
		return defaultYes
	}

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

	// Check if our trailer is present (ParseCheckpoint validates format, so found==true means valid)
	if _, found := trailers.ParseCheckpoint(message); !found {
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
	trailerPrefix := trailers.CheckpointTrailerKey + ":"
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
	trailerPrefix := trailers.CheckpointTrailerKey + ":"
	var result []string
	for _, line := range strings.Split(message, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), trailerPrefix) {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// isGitSequenceOperation checks if git is currently in the middle of a rebase,
// cherry-pick, or revert operation. During these operations, commits are being
// replayed and should not be linked to agent sessions.
//
// Detects:
//   - rebase: .git/rebase-merge/ or .git/rebase-apply/ directories
//   - cherry-pick: .git/CHERRY_PICK_HEAD file
//   - revert: .git/REVERT_HEAD file
func isGitSequenceOperation() bool {
	// Get git directory (handles worktrees and relative paths correctly)
	gitDir, err := GetGitDir()
	if err != nil {
		return false // Can't determine, assume not in sequence operation
	}

	// Check for rebase state directories
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-apply")); err == nil {
		return true
	}

	// Check for cherry-pick and revert state files
	if _, err := os.Stat(filepath.Join(gitDir, "CHERRY_PICK_HEAD")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(gitDir, "REVERT_HEAD")); err == nil {
		return true
	}

	return false
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
//   - "merge", "squash": skip trailer entirely (auto-generated messages)
//   - "commit": amend operation - preserves existing trailer or restores from LastCheckpointID
//

func (s *ManualCommitStrategy) PrepareCommitMsg(commitMsgFile string, source string) error {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	// Skip during rebase, cherry-pick, or revert operations
	// These are replaying existing commits and should not be linked to agent sessions
	if isGitSequenceOperation() {
		logging.Debug(logCtx, "prepare-commit-msg: skipped during git sequence operation",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
		)
		return nil
	}

	// Skip for merge and squash sources
	// These are auto-generated messages - not from Claude sessions
	switch source {
	case "merge", "squash":
		logging.Debug(logCtx, "prepare-commit-msg: skipped for source",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
		)
		return nil
	}

	// Handle amend (source="commit") separately: preserve or restore trailer
	if source == "commit" {
		return s.handleAmendCommitMsg(logCtx, commitMsgFile)
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

	// Fast path: when an agent is committing (ACTIVE session + no TTY), skip
	// content detection and interactive prompts. The agent can't respond to TTY
	// prompts and the content detection can miss mid-session work (no shadow
	// branch yet, transcript analysis may fail). Generate a checkpoint ID and
	// add the trailer directly.
	if !hasTTY() {
		for _, state := range sessions {
			if state.Phase.IsActive() {
				return s.addTrailerForAgentCommit(logCtx, commitMsgFile, state, source)
			}
		}
	}

	// Check if any session has new content to condense
	sessionsWithContent := s.filterSessionsWithNewContent(repo, sessions)

	if len(sessionsWithContent) == 0 {
		// No new content — no trailer needed
		logging.Debug(logCtx, "prepare-commit-msg: no content to link",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
			slog.Int("sessions_found", len(sessions)),
		)
		return nil
	}

	// Read current commit message
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	message := string(content)

	// Check if trailer already exists (ParseCheckpoint validates format, so found==true means valid)
	if existingCpID, found := trailers.ParseCheckpoint(message); found {
		// Trailer already exists (e.g., amend) - keep it
		logging.Debug(logCtx, "prepare-commit-msg: trailer already exists",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
			slog.String("existing_checkpoint_id", existingCpID.String()),
		)
		return nil
	}

	// Generate a fresh checkpoint ID
	checkpointID, err := id.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate checkpoint ID: %w", err)
	}

	// Determine agent type and last prompt from session
	agentType := DefaultAgentType // default for backward compatibility
	var lastPrompt string
	if len(sessionsWithContent) > 0 {
		firstSession := sessionsWithContent[0]
		if firstSession.AgentType != "" {
			agentType = firstSession.AgentType
		}
		lastPrompt = s.getLastPrompt(repo, firstSession)
	}

	// Prepare prompt for display: collapse newlines/whitespace, then truncate (rune-safe)
	displayPrompt := stringutil.TruncateRunes(stringutil.CollapseWhitespace(lastPrompt), 80, "...")

	// Add trailer differently based on commit source
	switch source {
	case "message":
		// Using -m or -F: ask user interactively whether to add trailer
		// (comments won't be stripped by git in this mode)

		// Build context string for interactive prompt
		var promptContext string
		if displayPrompt != "" {
			promptContext = "You have an active " + string(agentType) + " session.\nLast Prompt: " + displayPrompt
		}

		if !askConfirmTTY("Link this commit to "+string(agentType)+" session context?", promptContext, true) {
			// User declined - don't add trailer
			logging.Debug(logCtx, "prepare-commit-msg: user declined trailer",
				slog.String("strategy", "manual-commit"),
				slog.String("source", source),
			)
			return nil
		}
		message = addCheckpointTrailer(message, checkpointID)
	default:
		// Normal editor flow: add trailer with explanatory comment (will be stripped by git)
		message = addCheckpointTrailerWithComment(message, checkpointID, string(agentType), displayPrompt)
	}

	logging.Info(logCtx, "prepare-commit-msg: trailer added",
		slog.String("strategy", "manual-commit"),
		slog.String("source", source),
		slog.String("checkpoint_id", checkpointID.String()),
	)

	// Write updated message back
	if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	return nil
}

// handleAmendCommitMsg handles the prepare-commit-msg hook for amend operations
// (source="commit"). It preserves existing trailers or restores from LastCheckpointID.
func (s *ManualCommitStrategy) handleAmendCommitMsg(logCtx context.Context, commitMsgFile string) error {
	// Read current commit message
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	message := string(content)

	// If message already has a trailer, keep it unchanged
	if existingCpID, found := trailers.ParseCheckpoint(message); found {
		logging.Debug(logCtx, "prepare-commit-msg: amend preserves existing trailer",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_id", existingCpID.String()),
		)
		return nil
	}

	// No trailer in message — check if any session has LastCheckpointID to restore
	worktreePath, err := GetWorktreePath()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	sessions, err := s.findSessionsForWorktree(worktreePath)
	if err != nil || len(sessions) == 0 {
		return nil //nolint:nilerr // No sessions - nothing to restore
	}

	// For amend, HEAD^ is the commit being amended, and HEAD is where we are now.
	// We need to match sessions whose BaseCommit equals HEAD (the commit being amended
	// was created from this base). This prevents stale sessions from injecting
	// unrelated checkpoint IDs.
	repo, repoErr := OpenRepository()
	if repoErr != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}
	head, headErr := repo.Head()
	if headErr != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}
	currentHead := head.Hash().String()

	// Find first matching session with LastCheckpointID to restore.
	// LastCheckpointID is set after condensation completes.
	for _, state := range sessions {
		if state.BaseCommit != currentHead {
			continue
		}
		if state.LastCheckpointID.IsEmpty() {
			continue
		}
		cpID := state.LastCheckpointID
		source := "LastCheckpointID"

		// Restore the trailer
		message = addCheckpointTrailer(message, cpID)
		if writeErr := os.WriteFile(commitMsgFile, []byte(message), 0o600); writeErr != nil {
			return nil //nolint:nilerr // Hook must be silent on failure
		}

		logging.Info(logCtx, "prepare-commit-msg: restored trailer on amend",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_id", cpID.String()),
			slog.String("session_id", state.SessionID),
			slog.String("source", source),
		)
		return nil
	}

	// No checkpoint ID found - leave message unchanged
	logging.Debug(logCtx, "prepare-commit-msg: amend with no checkpoint to restore",
		slog.String("strategy", "manual-commit"),
	)
	return nil
}

// PostCommit is called by the git post-commit hook after a commit is created.
// Uses the session state machine to determine what action to take per session:
//   - ACTIVE → condense immediately (each commit gets its own checkpoint)
//   - IDLE → condense immediately
//   - ENDED → condense if files touched, discard if empty
//
// After condensation for ACTIVE sessions, remaining uncommitted files are
// carried forward to a new shadow branch so the next commit gets its own checkpoint.
//
// Shadow branches are only deleted when ALL sessions sharing the branch are non-active
// and were condensed during this PostCommit.
// During rebase/cherry-pick/revert operations, phase transitions are skipped entirely.
//
//nolint:unparam,maintidx // error return required by interface but hooks must return nil; maintidx: complex but already well-structured
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

	// Check if commit has checkpoint trailer (ParseCheckpoint validates format)
	checkpointID, found := trailers.ParseCheckpoint(commit.Message)
	if !found {
		// No trailer — user removed it or it was never added (mid-turn commit).
		// Still update BaseCommit for active sessions so future commits can match.
		s.postCommitUpdateBaseCommitOnly(logCtx, head)
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
			slog.String("checkpoint_id", checkpointID.String()),
		)
		return nil //nolint:nilerr // Intentional: hooks must be silent on failure
	}

	// Build transition context
	isRebase := isGitSequenceOperation()
	transitionCtx := session.TransitionContext{
		IsRebaseInProgress: isRebase,
	}

	if isRebase {
		logging.Debug(logCtx, "post-commit: rebase/sequence in progress, skipping phase transitions",
			slog.String("strategy", "manual-commit"),
		)
	}

	// Track shadow branch names and whether they can be deleted
	shadowBranchesToDelete := make(map[string]struct{})
	// Track active sessions that were NOT condensed — their shadow branches must be preserved
	uncondensedActiveOnBranch := make(map[string]bool)

	newHead := head.Hash().String()
	committedFileSet := filesChangedInCommit(commit)

	for _, state := range sessions {
		shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)

		// Check for new content (needed for TransitionContext and condensation).
		// Fail-open: if content check errors, assume new content exists so we
		// don't silently skip data that should have been condensed.
		//
		// For ACTIVE sessions: the commit has a checkpoint trailer (verified above),
		// meaning PrepareCommitMsg already determined this commit is session-related.
		// We trust that and assume hasNew = true, bypassing sessionHasNewContent which
		// would incorrectly return false (uses getStagedFiles, but files are no longer
		// staged after the commit).
		var hasNew bool
		if state.Phase.IsActive() {
			// For ACTIVE sessions, check if this session has any content to condense.
			// A session with no checkpoints (StepCount=0) and no files touched may exist
			// concurrently with other sessions that DO have content.
			// We only condense if this session actually has work.
			if state.StepCount > 0 || len(state.FilesTouched) > 0 {
				hasNew = true
			} else {
				// No checkpoints and no tracked files - check the live transcript.
				// Use sessionHasNewContentInCommittedFiles because staged files are empty
				// after the commit (files have already been committed).
				hasNew = s.sessionHasNewContentInCommittedFiles(state, committedFileSet)
			}
		} else {
			var contentErr error
			hasNew, contentErr = s.sessionHasNewContent(repo, state)
			if contentErr != nil {
				hasNew = true
				logging.Debug(logCtx, "post-commit: error checking session content, assuming new content",
					slog.String("session_id", state.SessionID),
					slog.String("error", contentErr.Error()),
				)
			}
		}
		transitionCtx.HasFilesTouched = len(state.FilesTouched) > 0

		// Run the state machine transition
		remaining := TransitionAndLog(state, session.EventGitCommit, transitionCtx)

		// Save FilesTouched BEFORE the action loop — condensation clears it,
		// but we need the original list for carry-forward computation.
		// For mid-session commits (ACTIVE, no shadow branch), state.FilesTouched may be empty
		// because no SaveChanges/Stop has been called yet. Extract files from transcript.
		filesTouchedBefore := make([]string, len(state.FilesTouched))
		copy(filesTouchedBefore, state.FilesTouched)
		if len(filesTouchedBefore) == 0 && state.Phase.IsActive() && state.TranscriptPath != "" {
			filesTouchedBefore = s.extractFilesFromLiveTranscript(state)
		}

		logging.Debug(logCtx, "post-commit: carry-forward prep",
			slog.String("session_id", state.SessionID),
			slog.Bool("is_active", state.Phase.IsActive()),
			slog.String("transcript_path", state.TranscriptPath),
			slog.Int("files_touched_before", len(filesTouchedBefore)),
			slog.Any("files", filesTouchedBefore),
		)

		condensed := false

		// Dispatch strategy-specific actions.
		// Each branch handles its own BaseCommit update so there is no
		// fallthrough conditional at the end. On condensation failure,
		// BaseCommit is intentionally NOT updated to preserve access to
		// the shadow branch (which is named after the old BaseCommit).
		for _, action := range remaining {
			switch action {
			case session.ActionCondense:
				// For ACTIVE sessions, any commit during the turn is session-related.
				// For IDLE/ENDED sessions (e.g., carry-forward), also require that the
				// committed files overlap with the session's remaining files AND have
				// matching content — otherwise an unrelated commit (or a commit with
				// completely replaced content) would incorrectly get this session's checkpoint.
				shouldCondense := hasNew
				if shouldCondense && !state.Phase.IsActive() {
					shouldCondense = filesOverlapWithContent(repo, shadowBranchName, commit, state.FilesTouched)
				}
				if shouldCondense {
					condensed = s.condenseAndUpdateState(logCtx, repo, checkpointID, state, head, shadowBranchName, shadowBranchesToDelete)
					// condenseAndUpdateState updates BaseCommit on success.
					// On failure, BaseCommit is preserved so the shadow branch remains accessible.
				} else {
					// No new content or unrelated commit — just update BaseCommit
					s.updateBaseCommitIfChanged(logCtx, state, newHead)
				}
			case session.ActionCondenseIfFilesTouched:
				// The state machine already gates this action on HasFilesTouched,
				// but hasNew is an additional content-level check (transcript has
				// new content beyond what was previously condensed).
				if len(state.FilesTouched) > 0 && hasNew {
					condensed = s.condenseAndUpdateState(logCtx, repo, checkpointID, state, head, shadowBranchName, shadowBranchesToDelete)
					// On failure, BaseCommit is preserved (same as ActionCondense).
				} else {
					s.updateBaseCommitIfChanged(logCtx, state, newHead)
				}
			case session.ActionDiscardIfNoFiles:
				if len(state.FilesTouched) == 0 {
					logging.Debug(logCtx, "post-commit: skipping empty ended session (no files to condense)",
						slog.String("session_id", state.SessionID),
					)
				}
				s.updateBaseCommitIfChanged(logCtx, state, newHead)
			case session.ActionClearEndedAt, session.ActionUpdateLastInteraction:
				// Handled by session.ApplyCommonActions above
			case session.ActionWarnStaleSession:
				// Not produced by EventGitCommit; listed for switch exhaustiveness
			}
		}

		// Record checkpoint ID for ACTIVE sessions so HandleTurnEnd can finalize
		// with full transcript. IDLE/ENDED sessions already have complete transcripts.
		// NOTE: This check runs AFTER TransitionAndLog updated the phase. It relies on
		// ACTIVE + GitCommit → ACTIVE (phase stays ACTIVE). If that state machine
		// transition ever changed, this guard would silently stop recording IDs.
		if condensed && state.Phase.IsActive() {
			state.TurnCheckpointIDs = append(state.TurnCheckpointIDs, checkpointID.String())
		}

		// Carry forward remaining uncommitted files so the next commit gets its
		// own checkpoint ID. This applies to ALL phases — if a user splits their
		// commit across two `git commit` invocations, each gets a 1:1 checkpoint.
		// Uses content-aware comparison: if user did `git add -p` and committed
		// partial changes, the file still has remaining agent changes to carry forward.
		if condensed {
			remainingFiles := filesWithRemainingAgentChanges(repo, shadowBranchName, commit, filesTouchedBefore, committedFileSet)
			logging.Debug(logCtx, "post-commit: carry-forward decision (content-aware)",
				slog.String("session_id", state.SessionID),
				slog.Int("files_touched_before", len(filesTouchedBefore)),
				slog.Int("committed_files", len(committedFileSet)),
				slog.Int("remaining_files", len(remainingFiles)),
				slog.Any("remaining", remainingFiles),
			)
			if len(remainingFiles) > 0 {
				s.carryForwardToNewShadowBranch(logCtx, repo, state, remainingFiles)
			}
		}

		// Save the updated state
		if err := s.saveSessionState(state); err != nil {
			fmt.Fprintf(os.Stderr, "[entire] Warning: failed to update session state: %v\n", err)
		}

		// Only preserve shadow branch for active sessions that were NOT condensed.
		// Condensed sessions already have their data on entire/checkpoints/v1.
		if state.Phase.IsActive() && !condensed {
			uncondensedActiveOnBranch[shadowBranchName] = true
		}
	}

	// Clean up shadow branches — only delete when ALL sessions on the branch are non-active
	// or were condensed during this PostCommit.
	for shadowBranchName := range shadowBranchesToDelete {
		if uncondensedActiveOnBranch[shadowBranchName] {
			logging.Debug(logCtx, "post-commit: preserving shadow branch (active session exists)",
				slog.String("shadow_branch", shadowBranchName),
			)
			continue
		}
		if err := deleteShadowBranch(repo, shadowBranchName); err != nil {
			fmt.Fprintf(os.Stderr, "[entire] Warning: failed to clean up %s: %v\n", shadowBranchName, err)
		} else {
			fmt.Fprintf(os.Stderr, "[entire] Cleaned up shadow branch: %s\n", shadowBranchName)
			logging.Info(logCtx, "shadow branch deleted",
				slog.String("strategy", "manual-commit"),
				slog.String("shadow_branch", shadowBranchName),
			)
		}
	}

	return nil
}

// condenseAndUpdateState runs condensation for a session and updates state afterward.
// Returns true if condensation succeeded.
func (s *ManualCommitStrategy) condenseAndUpdateState(
	logCtx context.Context,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	state *SessionState,
	head *plumbing.Reference,
	shadowBranchName string,
	shadowBranchesToDelete map[string]struct{},
) bool {
	result, err := s.CondenseSession(repo, checkpointID, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: condensation failed for session %s: %v\n",
			state.SessionID, err)
		logging.Warn(logCtx, "post-commit: condensation failed",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()),
		)
		return false
	}

	// Track this shadow branch for cleanup
	shadowBranchesToDelete[shadowBranchName] = struct{}{}

	// Update session state for the new base commit
	newHead := head.Hash().String()
	state.BaseCommit = newHead
	state.AttributionBaseCommit = newHead
	state.StepCount = 0
	state.CheckpointTranscriptStart = result.TotalTranscriptLines

	// Clear attribution tracking — condensation already used these values
	state.PromptAttributions = nil
	state.PendingPromptAttribution = nil
	state.FilesTouched = nil

	// Save checkpoint ID so subsequent commits can reuse it (e.g., amend restores trailer)
	state.LastCheckpointID = checkpointID

	shortID := state.SessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	fmt.Fprintf(os.Stderr, "[entire] Condensed session %s: %s (%d checkpoints)\n",
		shortID, result.CheckpointID, result.CheckpointsCount)

	logging.Info(logCtx, "session condensed",
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_id", result.CheckpointID.String()),
		slog.Int("checkpoints_condensed", result.CheckpointsCount),
		slog.Int("transcript_lines", result.TotalTranscriptLines),
	)

	return true
}

// updateBaseCommitIfChanged updates BaseCommit to newHead if it changed.
func (s *ManualCommitStrategy) updateBaseCommitIfChanged(logCtx context.Context, state *SessionState, newHead string) {
	if state.BaseCommit != newHead {
		state.BaseCommit = newHead
		logging.Debug(logCtx, "post-commit: updated BaseCommit",
			slog.String("session_id", state.SessionID),
			slog.String("new_head", truncateHash(newHead)),
		)
	}
}

// postCommitUpdateBaseCommitOnly updates BaseCommit for all sessions on the current
// worktree when a commit has no Entire-Checkpoint trailer. This prevents BaseCommit
// from going stale, which would cause future PrepareCommitMsg calls to skip the
// session (BaseCommit != currentHeadHash filter).
//
// Unlike the full PostCommit flow, this does NOT fire EventGitCommit or trigger
// condensation — it only keeps BaseCommit in sync with HEAD.
func (s *ManualCommitStrategy) postCommitUpdateBaseCommitOnly(logCtx context.Context, head *plumbing.Reference) {
	worktreePath, err := GetWorktreePath()
	if err != nil {
		return // Silent failure — hooks must be resilient
	}

	sessions, err := s.findSessionsForWorktree(worktreePath)
	if err != nil || len(sessions) == 0 {
		return
	}

	newHead := head.Hash().String()
	for _, state := range sessions {
		// Only update active sessions. Idle/ended sessions are kept around for
		// LastCheckpointID reuse and should not be advanced to HEAD.
		if !state.Phase.IsActive() {
			continue
		}
		if state.BaseCommit != newHead {
			logging.Debug(logCtx, "post-commit (no trailer): updating BaseCommit",
				slog.String("session_id", state.SessionID),
				slog.String("old_base", truncateHash(state.BaseCommit)),
				slog.String("new_head", truncateHash(newHead)),
			)
			state.BaseCommit = newHead
			if err := s.saveSessionState(state); err != nil {
				fmt.Fprintf(os.Stderr, "[entire] Warning: failed to update session state: %v\n", err)
			}
		}
	}
}

// truncateHash safely truncates a git hash to 7 chars for logging.
func truncateHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
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
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		// No shadow branch means no Stop has happened since the last condensation.
		// However, the agent may have done work (including commits) without a Stop.
		// Check the live transcript to detect this scenario.
		return s.sessionHasNewContentFromLiveTranscript(repo, state)
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
	var hasTranscriptFile bool

	if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileName); fileErr == nil {
		hasTranscriptFile = true
		if content, contentErr := file.Contents(); contentErr == nil {
			transcriptLines = countTranscriptItems(state.AgentType, content)
		}
	} else if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileNameLegacy); fileErr == nil {
		hasTranscriptFile = true
		if content, contentErr := file.Contents(); contentErr == nil {
			transcriptLines = countTranscriptItems(state.AgentType, content)
		}
	}

	// If shadow branch exists but has no transcript (e.g., carry-forward from mid-session commit),
	// check if the session has FilesTouched. Carry-forward sets FilesTouched with remaining files.
	if !hasTranscriptFile {
		if len(state.FilesTouched) > 0 {
			// Shadow branch has files from carry-forward - check if staged files overlap
			// AND have matching content (content-aware check).
			stagedFiles := getStagedFiles(repo)
			result := stagedFilesOverlapWithContent(repo, tree, stagedFiles, state.FilesTouched)
			return result, nil
		}
		// No transcript and no FilesTouched - fall back to live transcript check
		return s.sessionHasNewContentFromLiveTranscript(repo, state)
	}

	// Has new content if there are more lines than already condensed
	return transcriptLines > state.CheckpointTranscriptStart, nil
}

// sessionHasNewContentFromLiveTranscript checks if a session has new content
// by examining the live transcript file. This is used when no shadow branch exists
// (i.e., no Stop has happened yet) but the agent may have done work.
//
// Returns true if:
//  1. The transcript has grown since the last condensation, AND
//  2. The new transcript portion contains file modifications, AND
//  3. At least one modified file overlaps with the currently staged files
//
// The overlap check ensures we don't add checkpoint trailers to commits that are
// unrelated to the agent's recent changes.
//
// This handles the scenario where the agent commits mid-session before Stop.
func (s *ManualCommitStrategy) sessionHasNewContentFromLiveTranscript(repo *git.Repository, state *SessionState) (bool, error) {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	modifiedFiles, ok := s.extractNewModifiedFilesFromLiveTranscript(state)
	if !ok || len(modifiedFiles) == 0 {
		return false, nil
	}

	logging.Debug(logCtx, "live transcript check: found file modifications",
		slog.String("session_id", state.SessionID),
		slog.Int("modified_files", len(modifiedFiles)),
	)

	// Check if any modified files overlap with currently staged files
	// This ensures we only add checkpoint trailers to commits that include
	// files the agent actually modified
	stagedFiles := getStagedFiles(repo)

	logging.Debug(logCtx, "live transcript check: comparing staged vs modified",
		slog.String("session_id", state.SessionID),
		slog.Int("staged_files", len(stagedFiles)),
		slog.Int("modified_files", len(modifiedFiles)),
	)

	if !hasOverlappingFiles(stagedFiles, modifiedFiles) {
		logging.Debug(logCtx, "live transcript check: no overlap between staged and modified files",
			slog.String("session_id", state.SessionID),
		)
		return false, nil // No overlap - staged files are unrelated to agent's work
	}

	return true, nil
}

// sessionHasNewContentInCommittedFiles checks if a session has content that overlaps with
// the committed files. This is used in PostCommit for ACTIVE sessions where staged files
// are empty (already committed). Uses the live transcript to extract modified files and
// compares against the committed file set.
//
// KNOWN LIMITATION: This function relies on transcript analysis to detect file modifications.
// If the agent makes file modifications via shell commands (e.g., `sed`, `mv`, `cp`) that
// aren't captured in the transcript's file modification tracking, those modifications may
// not be detected. This is an acceptable edge case because:
// 1. Most agent file modifications use the Write/Edit tools which are tracked
// 2. Shell-based modifications are relatively rare in practice
// 3. The consequence (missing a checkpoint trailer) is minor - the transcript is still saved
func (s *ManualCommitStrategy) sessionHasNewContentInCommittedFiles(state *SessionState, committedFiles map[string]struct{}) bool {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	modifiedFiles, ok := s.extractNewModifiedFilesFromLiveTranscript(state)
	if !ok || len(modifiedFiles) == 0 {
		return false
	}

	logging.Debug(logCtx, "committed files check: found file modifications",
		slog.String("session_id", state.SessionID),
		slog.Int("modified_files", len(modifiedFiles)),
		slog.Int("committed_files", len(committedFiles)),
	)

	// Check if any modified files overlap with committed files
	for _, f := range modifiedFiles {
		if _, ok := committedFiles[f]; ok {
			return true
		}
	}

	logging.Debug(logCtx, "committed files check: no overlap between committed and modified files",
		slog.String("session_id", state.SessionID),
	)
	return false
}

// extractFilesFromLiveTranscript extracts modified file paths from the live transcript.
// Returns empty slice if extraction fails (fail-open behavior for hooks).
// Extracts ALL files from the transcript (offset 0) because this is used for carry-forward
// computation which needs to know all files touched, not just new ones.
func (s *ManualCommitStrategy) extractFilesFromLiveTranscript(state *SessionState) []string {
	return s.extractModifiedFilesFromLiveTranscript(state, 0)
}

// extractNewModifiedFilesFromLiveTranscript extracts modified files from the live
// transcript that are NEW since the last condensation. Returns the normalized file list
// and whether the extraction succeeded. Used by sessionHasNewContentFromLiveTranscript
// and sessionHasNewContentInCommittedFiles to detect agent work.
func (s *ManualCommitStrategy) extractNewModifiedFilesFromLiveTranscript(state *SessionState) ([]string, bool) {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	if state.TranscriptPath == "" || state.AgentType == "" {
		return nil, false
	}

	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		return nil, false
	}

	analyzer, ok := ag.(agent.TranscriptAnalyzer)
	if !ok {
		return nil, false
	}

	// Check if transcript has grown since last condensation
	currentPos, err := analyzer.GetTranscriptPosition(state.TranscriptPath)
	if err != nil {
		return nil, false
	}
	if currentPos <= state.CheckpointTranscriptStart {
		logging.Debug(logCtx, "live transcript check: no new content",
			slog.String("session_id", state.SessionID),
			slog.Int("current_pos", currentPos),
			slog.Int("start_offset", state.CheckpointTranscriptStart),
		)
		return nil, true // No new content, but extraction succeeded
	}

	return s.extractModifiedFilesFromLiveTranscript(state, state.CheckpointTranscriptStart), true
}

// extractModifiedFilesFromLiveTranscript extracts modified files from the live transcript
// (including subagent transcripts) starting from the given offset, and normalizes them
// to repo-relative paths. Returns the normalized file list.
func (s *ManualCommitStrategy) extractModifiedFilesFromLiveTranscript(state *SessionState, offset int) []string {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	if state.TranscriptPath == "" || state.AgentType == "" {
		return nil
	}

	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		return nil
	}

	analyzer, ok := ag.(agent.TranscriptAnalyzer)
	if !ok {
		return nil
	}

	modifiedFiles, _, err := analyzer.ExtractModifiedFilesFromOffset(state.TranscriptPath, offset)
	if err != nil {
		logging.Debug(logCtx, "extractModifiedFilesFromLiveTranscript: main transcript extraction failed",
			slog.String("transcript_path", state.TranscriptPath),
			slog.Any("error", err),
		)
	}

	// Also check subagent transcripts (Claude Code specific).
	// Subagents spawned via the Task tool write to separate transcript files.
	if state.AgentType == agent.AgentTypeClaudeCode {
		subagentsDir := filepath.Join(filepath.Dir(state.TranscriptPath), state.SessionID, "subagents")
		allFiles, extractErr := claudecode.ExtractAllModifiedFiles(state.TranscriptPath, offset, subagentsDir)
		if extractErr != nil {
			logging.Debug(logCtx, "extractModifiedFilesFromLiveTranscript: subagent extraction failed",
				slog.String("session_id", state.SessionID),
				slog.String("error", extractErr.Error()),
			)
		} else if len(allFiles) > len(modifiedFiles) {
			modifiedFiles = allFiles
		}
	}

	if len(modifiedFiles) == 0 {
		return nil
	}

	// Normalize to repo-relative paths.
	// Transcript tool_use entries contain absolute paths (e.g., /Users/alex/project/src/main.go)
	// but getStagedFiles/committedFiles use repo-relative paths (e.g., src/main.go).
	basePath := state.WorktreePath
	if basePath == "" {
		if wp, wpErr := GetWorktreePath(); wpErr == nil {
			basePath = wp
		}
	}
	if basePath != "" {
		normalized := make([]string, 0, len(modifiedFiles))
		for _, f := range modifiedFiles {
			if rel := paths.ToRelativePath(f, basePath); rel != "" {
				normalized = append(normalized, rel)
			} else {
				normalized = append(normalized, f)
			}
		}
		modifiedFiles = normalized
	}

	return modifiedFiles
}

// addTrailerForAgentCommit handles the fast path when an agent is committing
// (ACTIVE session + no TTY). Generates a checkpoint ID and adds the trailer
// directly, bypassing content detection and interactive prompts.
func (s *ManualCommitStrategy) addTrailerForAgentCommit(logCtx context.Context, commitMsgFile string, state *SessionState, source string) error {
	cpID, err := id.Generate()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	message := string(content)

	// Don't add if trailer already exists
	if _, found := trailers.ParseCheckpoint(message); found {
		return nil
	}

	message = addCheckpointTrailer(message, cpID)

	logging.Info(logCtx, "prepare-commit-msg: agent commit trailer added",
		slog.String("strategy", "manual-commit"),
		slog.String("source", source),
		slog.String("checkpoint_id", cpID.String()),
		slog.String("session_id", state.SessionID),
	)

	if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}
	return nil
}

// addCheckpointTrailer adds the Entire-Checkpoint trailer to a commit message.
// Handles proper trailer formatting (blank line before trailers if needed).
func addCheckpointTrailer(message string, checkpointID id.CheckpointID) string {
	trailer := trailers.CheckpointTrailerKey + ": " + checkpointID.String()

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
func addCheckpointTrailerWithComment(message string, checkpointID id.CheckpointID, agentName, prompt string) string {
	trailer := trailers.CheckpointTrailerKey + ": " + checkpointID.String()
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
// If there's an existing shadow branch with commits from a different session ID,
// returns a SessionIDConflictError to prevent orphaning existing session work.
//
// agentType is the human-readable name of the agent (e.g., "Claude Code").
// transcriptPath is the path to the live transcript file (for mid-session commit detection).
// userPrompt is the user's prompt text (stored truncated as FirstPrompt for display).
func (s *ManualCommitStrategy) InitializeSession(sessionID string, agentType agent.AgentType, transcriptPath string, userPrompt string) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Check if session already exists
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		return fmt.Errorf("failed to check session state: %w", err)
	}

	if state != nil && state.BaseCommit != "" {
		// Session is fully initialized — apply phase transition for TurnStart
		TransitionAndLog(state, session.EventTurnStart, session.TransitionContext{})

		// Generate a new TurnID for each turn (correlates carry-forward checkpoints)
		turnID, err := id.Generate()
		if err != nil {
			return fmt.Errorf("failed to generate turn ID: %w", err)
		}
		state.TurnID = turnID.String()

		// Backfill AgentType if empty or set to the generic default "Agent"
		if !isSpecificAgentType(state.AgentType) && agentType != "" {
			state.AgentType = agentType
		}

		// Backfill FirstPrompt if empty (for sessions created before the first_prompt field was added)
		if state.FirstPrompt == "" && userPrompt != "" {
			state.FirstPrompt = truncatePromptForStorage(userPrompt)
		}

		// Update transcript path if provided (may change on session resume)
		if transcriptPath != "" && state.TranscriptPath != transcriptPath {
			state.TranscriptPath = transcriptPath
		}

		// Clear checkpoint IDs on every new prompt.
		// LastCheckpointID is set during PostCommit, cleared at new prompt.
		// TurnCheckpointIDs tracks mid-turn checkpoints for stop-time finalization.
		state.LastCheckpointID = ""
		state.TurnCheckpointIDs = nil

		// Calculate attribution at prompt start (BEFORE agent makes any changes)
		// This captures user edits since the last checkpoint (or base commit for first prompt).
		// IMPORTANT: Always calculate attribution, even for the first checkpoint, to capture
		// user edits made before the first prompt. The inner CalculatePromptAttribution handles
		// nil lastCheckpointTree by falling back to baseTree.
		promptAttr := s.calculatePromptAttributionAtStart(repo, state)
		state.PendingPromptAttribution = &promptAttr

		// Check if HEAD has moved (user pulled/rebased or committed)
		// migrateShadowBranchIfNeeded handles renaming the shadow branch and updating state.BaseCommit
		if _, err := s.migrateShadowBranchIfNeeded(repo, state); err != nil {
			return fmt.Errorf("failed to check/migrate shadow branch: %w", err)
		}

		if err := s.saveSessionState(state); err != nil {
			return fmt.Errorf("failed to update session state: %w", err)
		}
		return nil
	}
	// If state exists but BaseCommit is empty, it's a partial state from concurrent warning
	// Continue below to properly initialize it

	// Initialize new session
	state, err = s.initializeSession(repo, sessionID, agentType, transcriptPath, userPrompt)
	if err != nil {
		return fmt.Errorf("failed to initialize session: %w", err)
	}

	// Apply phase transition: new session starts as ACTIVE
	TransitionAndLog(state, session.EventTurnStart, session.TransitionContext{})

	// Calculate attribution for pre-prompt edits
	// This captures any user edits made before the first prompt
	promptAttr := s.calculatePromptAttributionAtStart(repo, state)
	state.PendingPromptAttribution = &promptAttr
	if err = s.saveSessionState(state); err != nil {
		return fmt.Errorf("failed to save attribution: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Initialized shadow session: %s\n", sessionID)
	return nil
}

// calculatePromptAttributionAtStart calculates attribution at prompt start (before agent runs).
// This captures user changes since the last checkpoint - no filtering needed since
// the agent hasn't made any changes yet.
//
// IMPORTANT: This reads from the worktree (not staging area) to match what WriteTemporary
// captures in checkpoints. If we read staged content but checkpoints capture worktree content,
// unstaged changes would be in the checkpoint but not counted in PromptAttribution, causing
// them to be incorrectly attributed to the agent later.
func (s *ManualCommitStrategy) calculatePromptAttributionAtStart(
	repo *git.Repository,
	state *SessionState,
) PromptAttribution {
	logCtx := logging.WithComponent(context.Background(), "attribution")
	nextCheckpointNum := state.StepCount + 1
	result := PromptAttribution{CheckpointNumber: nextCheckpointNum}

	// Get last checkpoint tree from shadow branch (if it exists)
	// For the first checkpoint, no shadow branch exists yet - this is fine,
	// CalculatePromptAttribution will use baseTree as the reference instead.
	var lastCheckpointTree *object.Tree
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		logging.Debug(logCtx, "prompt attribution: no shadow branch yet (first checkpoint)",
			slog.String("shadow_branch", shadowBranchName))
		// Continue with lastCheckpointTree = nil
	} else {
		shadowCommit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			logging.Debug(logCtx, "prompt attribution: failed to get shadow commit",
				slog.String("shadow_ref", ref.Hash().String()),
				slog.String("error", err.Error()))
			// Continue with lastCheckpointTree = nil
		} else {
			lastCheckpointTree, err = shadowCommit.Tree()
			if err != nil {
				logging.Debug(logCtx, "prompt attribution: failed to get shadow tree",
					slog.String("error", err.Error()))
				// Continue with lastCheckpointTree = nil
			}
		}
	}

	// Get base tree for agent lines calculation
	var baseTree *object.Tree
	if baseCommit, err := repo.CommitObject(plumbing.NewHash(state.BaseCommit)); err == nil {
		if tree, treeErr := baseCommit.Tree(); treeErr == nil {
			baseTree = tree
		} else {
			logging.Debug(logCtx, "prompt attribution: base tree unavailable",
				slog.String("error", treeErr.Error()))
		}
	} else {
		logging.Debug(logCtx, "prompt attribution: base commit unavailable",
			slog.String("base_commit", state.BaseCommit),
			slog.String("error", err.Error()))
	}

	worktree, err := repo.Worktree()
	if err != nil {
		logging.Debug(logCtx, "prompt attribution skipped: failed to get worktree",
			slog.String("error", err.Error()))
		return result
	}

	// Get worktree status to find ALL changed files
	status, err := worktree.Status()
	if err != nil {
		logging.Debug(logCtx, "prompt attribution skipped: failed to get worktree status",
			slog.String("error", err.Error()))
		return result
	}

	worktreeRoot := worktree.Filesystem.Root()

	// Build map of changed files with their worktree content
	// IMPORTANT: We read from worktree (not staging area) to match what WriteTemporary
	// captures in checkpoints. This ensures attribution is consistent.
	changedFiles := make(map[string]string)
	for filePath, fileStatus := range status {
		// Skip unmodified files
		if fileStatus.Worktree == git.Unmodified && fileStatus.Staging == git.Unmodified {
			continue
		}
		// Skip .entire metadata directory (session data, not user code)
		if strings.HasPrefix(filePath, paths.EntireMetadataDir+"/") || strings.HasPrefix(filePath, ".entire/") {
			continue
		}

		// Always read from worktree to match checkpoint behavior
		fullPath := filepath.Join(worktreeRoot, filePath)
		var content string
		if data, err := os.ReadFile(fullPath); err == nil { //nolint:gosec // filePath is from git worktree status
			// Use git's binary detection algorithm (matches getFileContent behavior).
			// Binary files are excluded from line-based attribution calculations.
			isBinary, binErr := binary.IsBinary(bytes.NewReader(data))
			if binErr == nil && !isBinary {
				content = string(data)
			}
		}
		// else: file deleted, unreadable, or binary - content remains empty string

		changedFiles[filePath] = content
	}

	// Use CalculatePromptAttribution from manual_commit_attribution.go
	result = CalculatePromptAttribution(baseTree, lastCheckpointTree, changedFiles, nextCheckpointNum)

	return result
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
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	// Extract session data to get prompts for commit message generation
	// Pass agent type to handle different transcript formats (JSONL for Claude, JSON for Gemini)
	// Pass 0 for checkpointTranscriptStart since we're extracting all prompts, not calculating token usage
	sessionData, err := s.extractSessionData(repo, ref.Hash(), state.SessionID, nil, state.AgentType, "", 0)
	if err != nil || len(sessionData.Prompts) == 0 {
		return ""
	}

	// Return the last prompt (most recent work before commit)
	return sessionData.Prompts[len(sessionData.Prompts)-1]
}

// HandleTurnEnd dispatches strategy-specific actions emitted when an agent turn ends.
// The primary job is to finalize all checkpoints from this turn with the full transcript.
//
// During a turn, PostCommit writes provisional transcript data (whatever was available
// at commit time). HandleTurnEnd replaces that with the complete session transcript
// (from prompt to stop event), ensuring every checkpoint has the full context.
//
//nolint:unparam // error return required by interface but hooks must return nil
func (s *ManualCommitStrategy) HandleTurnEnd(state *SessionState) error {
	// Finalize all checkpoints from this turn with the full transcript.
	//
	// IMPORTANT: This is best-effort - errors are logged but don't fail the hook.
	// Failing here would prevent session cleanup and could leave state inconsistent.
	// The provisional transcript from PostCommit is already persisted, so the
	// checkpoint isn't lost - it just won't have the complete transcript.
	errCount := s.finalizeAllTurnCheckpoints(state)
	if errCount > 0 {
		logCtx := logging.WithComponent(context.Background(), "checkpoint")
		logging.Warn(logCtx, "HandleTurnEnd completed with errors (best-effort)",
			slog.String("session_id", state.SessionID),
			slog.Int("error_count", errCount),
		)
	}
	return nil
}

// finalizeAllTurnCheckpoints replaces the provisional transcript in each checkpoint
// created during this turn with the full session transcript.
//
// This is called at turn end (stop hook). During the turn, PostCommit wrote whatever
// transcript was available at commit time. Now we have the complete transcript and
// replace it so every checkpoint has the full prompt-to-stop context.
//
// Returns the number of errors encountered (best-effort: continues processing on error).
func (s *ManualCommitStrategy) finalizeAllTurnCheckpoints(state *SessionState) int {
	if len(state.TurnCheckpointIDs) == 0 {
		return 0 // No mid-turn commits to finalize
	}

	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	logging.Info(logCtx, "finalizing turn checkpoints with full transcript",
		slog.String("session_id", state.SessionID),
		slog.Int("checkpoint_count", len(state.TurnCheckpointIDs)),
	)

	errCount := 0

	// Read full transcript from live transcript file
	if state.TranscriptPath == "" {
		logging.Warn(logCtx, "finalize: no transcript path, skipping",
			slog.String("session_id", state.SessionID),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}

	fullTranscript, err := os.ReadFile(state.TranscriptPath)
	if err != nil || len(fullTranscript) == 0 {
		logging.Warn(logCtx, "finalize: failed to read transcript, skipping",
			slog.String("session_id", state.SessionID),
			slog.String("transcript_path", state.TranscriptPath),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}

	// Extract prompts and context from the full transcript
	prompts := extractUserPrompts(state.AgentType, string(fullTranscript))
	contextBytes := generateContextFromPrompts(prompts)

	// Redact secrets before writing — matches WriteCommitted behavior.
	// The live transcript on disk contains raw content; redaction must happen
	// before anything is persisted to the metadata branch.
	fullTranscript, err = redact.JSONLBytes(fullTranscript)
	if err != nil {
		logging.Warn(logCtx, "finalize: transcript redaction failed, skipping",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}
	for i, p := range prompts {
		prompts[i] = redact.String(p)
	}
	contextBytes = redact.Bytes(contextBytes)

	// Open repository and create checkpoint store
	repo, err := OpenRepository()
	if err != nil {
		logging.Warn(logCtx, "finalize: failed to open repository",
			slog.String("error", err.Error()),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}
	store := checkpoint.NewGitStore(repo)

	// Update each checkpoint with the full transcript
	for _, cpIDStr := range state.TurnCheckpointIDs {
		cpID, parseErr := id.NewCheckpointID(cpIDStr)
		if parseErr != nil {
			logging.Warn(logCtx, "finalize: invalid checkpoint ID, skipping",
				slog.String("checkpoint_id", cpIDStr),
				slog.String("error", parseErr.Error()),
			)
			errCount++
			continue
		}

		updateErr := store.UpdateCommitted(context.Background(), checkpoint.UpdateCommittedOptions{
			CheckpointID: cpID,
			SessionID:    state.SessionID,
			Transcript:   fullTranscript,
			Prompts:      prompts,
			Context:      contextBytes,
			Agent:        state.AgentType,
		})
		if updateErr != nil {
			logging.Warn(logCtx, "finalize: failed to update checkpoint",
				slog.String("checkpoint_id", cpIDStr),
				slog.String("error", updateErr.Error()),
			)
			errCount++
			continue
		}

		logging.Info(logCtx, "finalize: checkpoint updated with full transcript",
			slog.String("checkpoint_id", cpIDStr),
			slog.String("session_id", state.SessionID),
		)
	}

	// Update transcript start and clear turn checkpoint IDs
	fullTranscriptLines := countTranscriptItems(state.AgentType, string(fullTranscript))
	state.CheckpointTranscriptStart = fullTranscriptLines
	state.TurnCheckpointIDs = nil

	return errCount
}

// filesChangedInCommit returns the set of files changed in a commit by diffing against its parent.
func filesChangedInCommit(commit *object.Commit) map[string]struct{} {
	result := make(map[string]struct{})

	commitTree, err := commit.Tree()
	if err != nil {
		return result
	}

	var parentTree *object.Tree
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err != nil {
			return result
		}
		parentTree, err = parent.Tree()
		if err != nil {
			return result
		}
	}

	if parentTree == nil {
		// Initial commit — all files are new
		if iterErr := commitTree.Files().ForEach(func(f *object.File) error {
			result[f.Name] = struct{}{}
			return nil
		}); iterErr != nil {
			return result
		}
		return result
	}

	changes, err := parentTree.Diff(commitTree)
	if err != nil {
		return result
	}
	for _, change := range changes {
		name := change.To.Name
		if name == "" {
			name = change.From.Name
		}
		result[name] = struct{}{}
	}
	return result
}

// subtractFiles returns files that are NOT in the exclude set.
func subtractFiles(files []string, exclude map[string]struct{}) []string {
	var remaining []string
	for _, f := range files {
		if _, excluded := exclude[f]; !excluded {
			remaining = append(remaining, f)
		}
	}
	return remaining
}

// carryForwardToNewShadowBranch creates a new shadow branch at the current HEAD
// containing the remaining uncommitted files and all session metadata.
// This enables the next commit to get its own unique checkpoint.
func (s *ManualCommitStrategy) carryForwardToNewShadowBranch(
	logCtx context.Context,
	repo *git.Repository,
	state *SessionState,
	remainingFiles []string,
) {
	store := checkpoint.NewGitStore(repo)

	// Don't include metadata directory in carry-forward. The carry-forward branch
	// only needs to preserve file content for comparison - not the transcript.
	// Including the transcript would cause sessionHasNewContent to always return true
	// because CheckpointTranscriptStart is reset to 0 for carry-forward.
	result, err := store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
		SessionID:         state.SessionID,
		BaseCommit:        state.BaseCommit,
		WorktreeID:        state.WorktreeID,
		ModifiedFiles:     remainingFiles,
		MetadataDir:       "",
		MetadataDirAbs:    "",
		CommitMessage:     "carry forward: uncommitted session files",
		IsFirstCheckpoint: false,
	})
	if err != nil {
		logging.Warn(logCtx, "post-commit: carry-forward failed",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()),
		)
		return
	}
	if result.Skipped {
		logging.Debug(logCtx, "post-commit: carry-forward skipped (no changes)",
			slog.String("session_id", state.SessionID),
		)
		return
	}

	// Update state for the carry-forward checkpoint.
	// CheckpointTranscriptStart = 0 is intentional: each checkpoint is self-contained with
	// the full transcript. This trades storage efficiency for simplicity:
	// - Pro: Each checkpoint is independently readable without needing to stitch together
	//   multiple checkpoints to understand the session history
	// - Con: For long sessions with multiple partial commits, each checkpoint includes
	//   the full transcript, which could be large
	// An alternative would be incremental checkpoints (only new content since last condensation),
	// but this would complicate checkpoint retrieval and require careful tracking of dependencies.
	state.FilesTouched = remainingFiles
	state.StepCount = 1
	state.CheckpointTranscriptStart = 0
	state.LastCheckpointID = ""
	// NOTE: TurnCheckpointIDs is intentionally NOT cleared here. Those checkpoint
	// IDs from earlier in the turn still need finalization with the full transcript
	// when HandleTurnEnd runs at stop time.

	logging.Info(logCtx, "post-commit: carried forward remaining files",
		slog.String("session_id", state.SessionID),
		slog.Int("remaining_files", len(remainingFiles)),
	)
}
