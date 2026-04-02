package strategy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitops"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/utils/binary"
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

	// Gemini CLI sets GEMINI_CLI=1 when running shell commands.
	// Gemini subprocesses may have access to the user's TTY, but they can't
	// actually respond to interactive prompts. Treat them as non-TTY.
	// See: https://geminicli.com/docs/tools/shell/
	if os.Getenv("GEMINI_CLI") != "" {
		return false
	}

	// Copilot CLI sets COPILOT_CLI=1 when running hook subprocesses (v0.0.421+).
	// Like Gemini, the subprocess may inherit the user's TTY but can't respond
	// to interactive prompts.
	if os.Getenv("COPILOT_CLI") != "" {
		return false
	}

	// GIT_TERMINAL_PROMPT=0 disables git's own terminal prompts.
	// Factory AI Droid (and other non-interactive environments like CI) set this.
	// Since we run as a git hook, respect it — if the environment doesn't want
	// git prompting, our hook shouldn't prompt either.
	if os.Getenv("GIT_TERMINAL_PROMPT") == "0" {
		return false
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// ttyResult represents the outcome of a TTY confirmation prompt.
type ttyResult int

const (
	ttyResultLink       ttyResult = iota // Link: add the checkpoint trailer
	ttyResultSkip                        // Skip: don't add the trailer
	ttyResultLinkAlways                  // Link and remember: add trailer + save "always" preference
)

// askConfirmTTY prompts the user via /dev/tty whether to link a commit to session context.
// This requires a controlling terminal — callers must check hasTTY() first and handle
// the no-TTY case (agent subprocesses, CI) themselves.
//
// header is displayed as the first line (e.g., "Entire: Active Claude Code session").
// detail lines are displayed indented below the header.
func askConfirmTTY(header string, details []string, prompt string, defaultYes bool) ttyResult {
	defaultResult := ttyResultSkip
	if defaultYes {
		defaultResult = ttyResultLink
	}

	// In test mode, don't try to interact with the real TTY — just use the default.
	// ENTIRE_TEST_TTY=1 simulates "a human is present" for the hasTTY() check
	// but we can't actually read from the TTY in tests.
	if os.Getenv("ENTIRE_TEST_TTY") != "" {
		return defaultResult
	}

	// Open /dev/tty for both reading and writing.
	// This is the controlling terminal, which works even when stdin/stderr are redirected
	// (e.g., human runs git commit -m where stdin is not a pipe).
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return defaultResult
	}
	defer tty.Close()

	// Write to tty directly, not stderr, since git hooks may redirect stderr to /dev/null
	fmt.Fprintf(tty, "\n%s\n", header)
	for _, line := range details {
		fmt.Fprintf(tty, "  %s\n", line)
	}

	// Show prompt with option descriptions
	fmt.Fprintf(tty, "\n%s\n", prompt)
	if defaultYes {
		fmt.Fprint(tty, "  [Y]es / [n]o / [a]lways (remember my choice): ")
	} else {
		fmt.Fprint(tty, "  [y]es / [N]o / [a]lways (remember my choice): ")
	}

	// Read response
	reader := bufio.NewReader(tty)
	response, err := reader.ReadString('\n')
	if err != nil {
		return defaultResult
	}

	response = strings.TrimSpace(strings.ToLower(response))
	switch response {
	case "y", "yes":
		return ttyResultLink
	case "n", "no":
		return ttyResultSkip
	case "a", "always":
		return ttyResultLinkAlways
	default:
		// Empty or invalid input - use default
		return defaultResult
	}
}

// saveCommitLinkingAlways persists commit_linking = "always" to settings.local.json.
// Uses raw JSON merge to set only the commit_linking field without affecting other
// fields. This avoids writing unintended defaults (e.g., enabled: true) when the
// local settings file doesn't exist yet.
func saveCommitLinkingAlways(ctx context.Context) error {
	localPath, err := paths.AbsPath(ctx, settings.EntireSettingsLocalFile)
	if err != nil {
		return fmt.Errorf("resolving local settings path: %w", err)
	}

	// Read existing file as raw JSON map to preserve all existing fields.
	// If the file doesn't exist, start with an empty map so we only write commit_linking.
	var raw map[string]json.RawMessage
	data, readErr := os.ReadFile(localPath) //nolint:gosec // path is from AbsPath
	if readErr == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parsing local settings: %w", err)
		}
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("reading local settings: %w", readErr)
	}
	if raw == nil {
		raw = make(map[string]json.RawMessage)
	}

	raw["commit_linking"] = json.RawMessage(`"` + settings.CommitLinkingAlways + `"`)

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling local settings: %w", err)
	}
	out = append(out, '\n')

	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return fmt.Errorf("creating settings directory: %w", err)
	}
	//nolint:gosec // G306: settings file is config, not secrets; 0o644 is appropriate
	if err := os.WriteFile(localPath, out, 0o644); err != nil {
		return fmt.Errorf("writing local settings: %w", err)
	}
	return nil
}

// CommitMsg is called by the git commit-msg hook after the user edits the message.
// If the message contains only our trailer (no actual user content), strip it
// so git will abort the commit due to empty message.

func (s *ManualCommitStrategy) CommitMsg(_ context.Context, commitMsgFile string) error { //nolint:unparam // error return is part of the hook contract; callers check it
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
func isGitSequenceOperation(ctx context.Context) bool {
	// Get git directory (handles worktrees and relative paths correctly)
	gitDir, err := GetGitDir(ctx)
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

func (s *ManualCommitStrategy) PrepareCommitMsg(ctx context.Context, commitMsgFile string, source string) error {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	// Skip during rebase, cherry-pick, or revert operations
	// These are replaying existing commits and should not be linked to agent sessions
	if isGitSequenceOperation(ctx) {
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
		return s.handleAmendCommitMsg(ctx, commitMsgFile)
	}

	_, openRepoSpan := perf.Start(ctx, "open_repository")
	repo, err := OpenRepository(ctx)
	if err != nil {
		openRepoSpan.RecordError(err)
		openRepoSpan.End()
		return nil
	}
	openRepoSpan.End()

	_, findSessionsSpan := perf.Start(ctx, "find_sessions_for_worktree")
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		findSessionsSpan.RecordError(err)
		findSessionsSpan.End()
		return nil
	}

	// Find all active sessions for this worktree
	// We match by worktree (not BaseCommit) because the user may have made
	// intermediate commits without entering new prompts, causing HEAD to diverge
	sessions, err := s.findSessionsForWorktree(ctx, worktreePath)
	if err != nil || len(sessions) == 0 {
		findSessionsSpan.RecordError(err)
		findSessionsSpan.End()
		// No active sessions or error listing - silently skip (hooks must be resilient)
		logging.Debug(logCtx, "prepare-commit-msg: no active sessions",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
		)
		return nil
	}
	findSessionsSpan.End()

	// Fast path: skip content detection for mid-turn agent commits.
	if s.tryAgentCommitFastPath(ctx, commitMsgFile, sessions, source) {
		return nil
	}

	// Check if any session has new content to condense
	_, filterSessionsSpan := perf.Start(ctx, "filter_sessions_with_content")
	sessionsWithContent := s.filterSessionsWithNewContent(ctx, repo, sessions)
	filterSessionsSpan.End()

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
	_, readCommitMessageSpan := perf.Start(ctx, "read_commit_message")
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		readCommitMessageSpan.RecordError(err)
		readCommitMessageSpan.End()
		return nil
	}

	message := string(content)

	// Check if trailer already exists (ParseCheckpoint validates format, so found==true means valid)
	if existingCpID, found := trailers.ParseCheckpoint(message); found {
		readCommitMessageSpan.End()
		// Trailer already exists (e.g., amend) - keep it
		logging.Debug(logCtx, "prepare-commit-msg: trailer already exists",
			slog.String("strategy", "manual-commit"),
			slog.String("source", source),
			slog.String("existing_checkpoint_id", existingCpID.String()),
		)
		return nil
	}
	readCommitMessageSpan.End()

	// Generate a fresh checkpoint ID and resolve session metadata
	_, resolveMetadataSpan := perf.Start(ctx, "resolve_session_metadata")
	checkpointID, err := id.Generate()
	if err != nil {
		resolveMetadataSpan.RecordError(err)
		resolveMetadataSpan.End()
		return fmt.Errorf("failed to generate checkpoint ID: %w", err)
	}

	// Determine agent type and last prompt from session
	var agentType types.AgentType
	var lastPrompt string
	if len(sessionsWithContent) > 0 {
		firstSession := sessionsWithContent[0]
		if firstSession.AgentType != "" {
			agentType = firstSession.AgentType
		}
		lastPrompt = s.getLastPrompt(ctx, repo, firstSession)
	}

	// Prepare prompt for display: collapse newlines/whitespace, then truncate (rune-safe)
	displayPrompt := stringutil.TruncateRunes(stringutil.CollapseWhitespace(lastPrompt), 80, "...")

	// Load commit_linking setting to decide whether to prompt
	commitLinking := settings.CommitLinkingPrompt // safe default
	if stngs, loadErr := settings.Load(ctx); loadErr == nil {
		commitLinking = stngs.GetCommitLinking()
	}
	resolveMetadataSpan.End()

	// Add trailer differently based on commit source
	// NOTE: TTY confirmation (askConfirmTTY) is intentionally NOT wrapped in a span
	// because it blocks on user input and would skew timing.
	switch source {
	case "message":
		// Using -m or -F: behavior depends on TTY availability and commit_linking setting
		switch {
		case !hasTTY():
			// No TTY (agent subprocess, CI) — auto-link without prompting
			message = addCheckpointTrailer(message, checkpointID)
		case commitLinking == settings.CommitLinkingAlways:
			// User previously chose "always" — auto-link without prompting
			message = addCheckpointTrailer(message, checkpointID)
		default:
			// Human at terminal — prompt interactively
			header := "Entire: Active " + string(agentType) + " session detected"
			var details []string
			if displayPrompt != "" {
				details = append(details, "Last prompt: "+displayPrompt)
			}

			result := askConfirmTTY(header, details, "Link this commit to session context?", true)
			if result == ttyResultSkip {
				logging.Debug(logCtx, "prepare-commit-msg: user declined trailer",
					slog.String("strategy", "manual-commit"),
					slog.String("source", source),
				)
				return nil
			}
			if result == ttyResultLinkAlways {
				// Persist preference so future commits auto-link (non-fatal if it fails)
				if saveErr := saveCommitLinkingAlways(ctx); saveErr != nil {
					logging.Warn(logCtx, "prepare-commit-msg: failed to save commit_linking=always",
						slog.String("error", saveErr.Error()),
					)
				}
			}
			message = addCheckpointTrailer(message, checkpointID)
		}
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
	_, writeCommitMessageSpan := perf.Start(ctx, "write_commit_message")
	if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil { //nolint:gosec // path from git hook arg
		writeCommitMessageSpan.RecordError(err)
		writeCommitMessageSpan.End()
		return nil
	}
	writeCommitMessageSpan.End()

	return nil
}

// handleAmendCommitMsg handles the prepare-commit-msg hook for amend operations
// (source="commit"). It preserves existing trailers or restores from LastCheckpointID.
func (s *ManualCommitStrategy) handleAmendCommitMsg(ctx context.Context, commitMsgFile string) error {
	logCtx := logging.WithComponent(ctx, "checkpoint")
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
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	sessions, err := s.findSessionsForWorktree(ctx, worktreePath)
	if err != nil || len(sessions) == 0 {
		return nil //nolint:nilerr // No sessions - nothing to restore
	}

	// For amend, HEAD^ is the commit being amended, and HEAD is where we are now.
	// We need to match sessions whose BaseCommit equals HEAD (the commit being amended
	// was created from this base). This prevents stale sessions from injecting
	// unrelated checkpoint IDs.
	repo, repoErr := OpenRepository(ctx)
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
		if writeErr := os.WriteFile(commitMsgFile, []byte(message), 0o600); writeErr != nil { //nolint:gosec // path from git hook arg
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

// postCommitActionHandler implements session.ActionHandler for PostCommit.
// Each session in the loop gets its own handler with per-session context.
// Handler methods use the *State parameter from ApplyTransition (same pointer
// as the state being transitioned) rather than capturing state separately.
type postCommitActionHandler struct {
	s                      *ManualCommitStrategy
	ctx                    context.Context
	repo                   *git.Repository
	checkpointID           id.CheckpointID
	head                   *plumbing.Reference
	commit                 *object.Commit
	newHead                string
	repoDir                string
	shadowBranchName       string
	shadowBranchesToDelete map[string]struct{}
	committedFileSet       map[string]struct{}
	hasNew                 bool
	filesTouchedBefore     []string

	// Cached git objects — resolved once per PostCommit invocation to avoid
	// redundant reads across filesOverlapWithContent, filesWithRemainingAgentChanges,
	// CondenseSession, and calculateSessionAttributions.
	headTree   *object.Tree        // HEAD commit tree (shared across all sessions)
	parentTree *object.Tree        // HEAD's first parent tree (shared, nil for initial commits)
	shadowRef  *plumbing.Reference // Per-session shadow branch ref (nil if branch doesn't exist)
	shadowTree *object.Tree        // Per-session shadow commit tree (nil if branch doesn't exist)

	// Output: set by handler methods, read by caller after TransitionAndLog.
	condensed bool
}

// parentCommitHash returns the first parent's hash as a string, or empty for initial commits.
func (h *postCommitActionHandler) parentCommitHash() string {
	if h.commit.NumParents() > 0 && len(h.commit.ParentHashes) > 0 {
		return h.commit.ParentHashes[0].String()
	}
	return ""
}

func (h *postCommitActionHandler) HandleCondense(state *session.State) error {
	logCtx := logging.WithComponent(h.ctx, "checkpoint")
	shouldCondense := h.shouldCondenseWithOverlapCheck(state.Phase.IsActive(), state.LastInteractionTime)

	logging.Debug(logCtx, "post-commit: HandleCondense decision",
		slog.String("session_id", state.SessionID),
		slog.String("phase", string(state.Phase)),
		slog.Bool("has_new", h.hasNew),
		slog.Bool("should_condense", shouldCondense),
		slog.String("shadow_branch", h.shadowBranchName),
	)

	if shouldCondense {
		h.condensed = h.s.condenseAndUpdateState(h.ctx, h.repo, h.checkpointID, state, h.head, h.shadowBranchName, h.shadowBranchesToDelete, h.committedFileSet, condenseOpts{
			shadowRef:        h.shadowRef,
			headTree:         h.headTree,
			parentTree:       h.parentTree,
			repoDir:          h.repoDir,
			parentCommitHash: h.parentCommitHash(),
			headCommitHash:   h.newHead,
		})
	} else {
		h.s.updateBaseCommitIfChanged(h.ctx, state, h.newHead)
	}
	return nil
}

func (h *postCommitActionHandler) HandleCondenseIfFilesTouched(state *session.State) error {
	logCtx := logging.WithComponent(h.ctx, "checkpoint")
	shouldCondense := len(state.FilesTouched) > 0 && h.shouldCondenseWithOverlapCheck(state.Phase.IsActive(), state.LastInteractionTime)

	logging.Debug(logCtx, "post-commit: HandleCondenseIfFilesTouched decision",
		slog.String("session_id", state.SessionID),
		slog.String("phase", string(state.Phase)),
		slog.Bool("has_new", h.hasNew),
		slog.Int("files_touched", len(state.FilesTouched)),
		slog.Bool("should_condense", shouldCondense),
		slog.String("shadow_branch", h.shadowBranchName),
	)

	if shouldCondense {
		h.condensed = h.s.condenseAndUpdateState(h.ctx, h.repo, h.checkpointID, state, h.head, h.shadowBranchName, h.shadowBranchesToDelete, h.committedFileSet, condenseOpts{
			shadowRef:        h.shadowRef,
			headTree:         h.headTree,
			parentTree:       h.parentTree,
			repoDir:          h.repoDir,
			parentCommitHash: h.parentCommitHash(),
			headCommitHash:   h.newHead,
		})
	} else {
		h.s.updateBaseCommitIfChanged(h.ctx, state, h.newHead)
	}
	return nil
}

// shouldCondenseWithOverlapCheck returns true if the session should be condensed
// into this commit. Active sessions with recent interaction always condense
// (bypasses overlap check). Stale ACTIVE and IDLE/ENDED sessions require
// file overlap evidence between tracked files and committed files.
func (h *postCommitActionHandler) shouldCondenseWithOverlapCheck(isActive bool, lastInteraction *time.Time) bool {
	if !h.hasNew {
		return false
	}
	// ACTIVE sessions with recent interaction: skip the overlap check.
	// PrepareCommitMsg already validated this commit is session-related
	// (added trailer). The overlap check is only meaningful when we need
	// heuristic evidence that a commit was related to the session.
	//
	// We check LastInteractionTime to avoid condensing stale ACTIVE sessions
	// (agent killed without Stop hook) into every subsequent commit. A stale
	// session has no recent interaction and falls through to the overlap check.
	if isActive && isRecentInteraction(lastInteraction) {
		return true
	}
	if len(h.filesTouchedBefore) == 0 {
		return false // No files tracked = no overlap evidence
	}
	// Only check files that were actually changed in this commit.
	// Without this, files that exist in the tree but weren't changed
	// would pass the "modified file" check in filesOverlapWithContent
	// (because the file exists in the parent tree), causing stale
	// sessions to be incorrectly condensed.
	var committedTouchedFiles []string
	for _, f := range h.filesTouchedBefore {
		if _, ok := h.committedFileSet[f]; ok {
			committedTouchedFiles = append(committedTouchedFiles, f)
		}
	}
	if len(committedTouchedFiles) == 0 {
		return false
	}
	return filesOverlapWithContent(h.ctx, h.repo, h.shadowBranchName, h.commit, committedTouchedFiles, overlapOpts{
		headTree:      h.headTree,
		shadowTree:    h.shadowTree,
		parentTree:    h.parentTree,
		hasParentTree: true,
	})
}

const (
	staleEndedSessionWarnThreshold = 3                   // warn when ≥ this many stale sessions
	staleEndedSessionWarnInterval  = 24 * time.Hour      // rate-limit window
	staleEndedSessionWarnFile      = ".warn-stale-ended" // sentinel file name in entire-sessions/
)

// stderrWriter is the destination for user-facing warnings emitted outside of
// Cobra command output (e.g. from PostCommit). Tests can swap this to capture
// output without mutating the process-global os.Stderr.
var stderrWriter io.Writer = os.Stderr

// warnStaleEndedSessions emits a rate-limited warning to stderr when too many
// non-FullyCondensed ENDED sessions are accumulating.
func warnStaleEndedSessions(ctx context.Context, count int) {
	warnStaleEndedSessionsTo(ctx, count, stderrWriter)
}

func warnStaleEndedSessionsTo(ctx context.Context, count int, w io.Writer) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return // fail-open
	}
	warnDir := filepath.Join(commonDir, session.SessionStateDirName)
	warnFile := filepath.Join(warnDir, staleEndedSessionWarnFile)
	if info, statErr := os.Stat(warnFile); statErr == nil {
		if time.Since(info.ModTime()) < staleEndedSessionWarnInterval {
			return // rate-limited
		}
	}
	//nolint:errcheck,gosec // G104: Best-effort warning — fail-open if file ops fail
	os.MkdirAll(warnDir, 0o750)
	//nolint:errcheck,gosec // G104: Best-effort sentinel file write
	os.WriteFile(warnFile, []byte{}, 0o644)
	fmt.Fprintf(w,
		"\nentire: %d ended session(s) are accumulating and slowing down commits.\n"+
			"Run 'entire doctor' to condense them and restore commit performance.\n\n",
		count,
	)
}

// activeSessionInteractionThreshold is the maximum age of LastInteractionTime
// for an ACTIVE session to be considered genuinely active. 24h is generous
// because LastInteractionTime only updates at TurnStart, not per-tool-call.
const activeSessionInteractionThreshold = 24 * time.Hour

// isRecentInteraction returns true if lastInteraction is non-nil and within
// activeSessionInteractionThreshold of now.
func isRecentInteraction(lastInteraction *time.Time) bool {
	return lastInteraction != nil && time.Since(*lastInteraction) < activeSessionInteractionThreshold
}

func (h *postCommitActionHandler) HandleDiscardIfNoFiles(state *session.State) error {
	if len(state.FilesTouched) == 0 {
		logging.Debug(logging.WithComponent(h.ctx, "checkpoint"), "post-commit: skipping empty ended session (no files to condense)",
			slog.String("session_id", state.SessionID),
		)
	}
	h.s.updateBaseCommitIfChanged(h.ctx, state, h.newHead)
	return nil
}

func (h *postCommitActionHandler) HandleWarnStaleSession(_ *session.State) error {
	// Not produced by EventGitCommit; no-op for exhaustiveness.
	return nil
}

// During rebase/cherry-pick/revert operations, phase transitions are skipped entirely.
//

func (s *ManualCommitStrategy) PostCommit(ctx context.Context) error { //nolint:unparam // error return is part of the hook contract; callers check it
	logCtx := logging.WithComponent(ctx, "checkpoint")

	_, openRepoSpan := perf.Start(ctx, "open_repository_and_head")
	repo, err := OpenRepository(ctx)
	if err != nil {
		openRepoSpan.RecordError(err)
		openRepoSpan.End()
		return nil
	}

	// Get HEAD commit to check for trailer
	head, err := repo.Head()
	if err != nil {
		openRepoSpan.RecordError(err)
		openRepoSpan.End()
		return nil
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		openRepoSpan.RecordError(err)
		openRepoSpan.End()
		return nil
	}

	// Check if commit has checkpoint trailer (ParseCheckpoint validates format)
	checkpointID, found := trailers.ParseCheckpoint(commit.Message)
	openRepoSpan.End()

	if !found {
		// No trailer — user removed it or it was never added (mid-turn commit).
		// Still update BaseCommit for active sessions so future commits can match.
		s.postCommitUpdateBaseCommitOnly(ctx, head)
		return nil
	}

	_, findSessionsSpan := perf.Start(ctx, "find_sessions_for_worktree")
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		findSessionsSpan.RecordError(err)
		findSessionsSpan.End()
		return nil
	}

	// Find all active sessions for this worktree
	sessions, err := s.findSessionsForWorktree(ctx, worktreePath)
	findSessionsSpan.RecordError(err)
	findSessionsSpan.End()

	if err != nil || len(sessions) == 0 {
		logging.Warn(logCtx, "post-commit: no active sessions despite trailer",
			slog.String("strategy", "manual-commit"),
			slog.String("checkpoint_id", checkpointID.String()),
		)
		return nil //nolint:nilerr // Intentional: hooks must be silent on failure
	}

	// Build transition context
	isRebase := isGitSequenceOperation(ctx)
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

	// Pre-resolve HEAD tree and parent tree once for the entire PostCommit.
	// These are immutable within this hook invocation and used by multiple
	// per-session functions (filesOverlapWithContent, filesWithRemainingAgentChanges,
	// calculateSessionAttributions).
	_, resolveTreesSpan := perf.Start(ctx, "resolve_commit_trees")
	var headTree *object.Tree
	if t, err := commit.Tree(); err == nil {
		headTree = t
	}
	var parentTree *object.Tree
	if commit.NumParents() > 0 {
		if parent, err := commit.Parent(0); err == nil {
			if t, err := parent.Tree(); err == nil {
				parentTree = t
			}
		}
	}

	committedFileSet := filesChangedInCommit(ctx, worktreePath, commit, headTree, parentTree)
	resolveTreesSpan.End()

	loopCtx, processSessionsLoop := perf.StartLoop(ctx, "process_sessions")
	for _, state := range sessions {
		// Skip fully-condensed ended sessions — no work remains.
		// These sessions only persist for LastCheckpointID (amend trailer reuse).
		if state.FullyCondensed && state.Phase == session.PhaseEnded {
			continue
		}
		iterCtx, iterSpan := processSessionsLoop.Iteration(loopCtx)
		s.postCommitProcessSession(iterCtx, repo, state, &transitionCtx, checkpointID,
			head, commit, newHead, worktreePath, headTree, parentTree, committedFileSet,
			shadowBranchesToDelete, uncondensedActiveOnBranch)
		iterSpan.End()
	}
	processSessionsLoop.End()

	// Clean up shadow branches — only delete when ALL sessions on the branch are non-active
	// or were condensed during this PostCommit.
	_, cleanupBranchesSpan := perf.Start(ctx, "cleanup_shadow_branches")
	for shadowBranchName := range shadowBranchesToDelete {
		if uncondensedActiveOnBranch[shadowBranchName] {
			logging.Debug(logCtx, "post-commit: preserving shadow branch (active session exists)",
				slog.String("shadow_branch", shadowBranchName),
			)
			continue
		}
		if err := deleteShadowBranch(ctx, repo, shadowBranchName); err != nil {
			logging.Warn(logCtx, "failed to clean up shadow branch",
				slog.String("shadow_branch", shadowBranchName),
				slog.String("error", err.Error()))
		} else {
			logging.Info(logCtx, "shadow branch deleted",
				slog.String("strategy", "manual-commit"),
				slog.String("shadow_branch", shadowBranchName),
			)
		}
	}
	cleanupBranchesSpan.End()

	if stale := countWarnableStaleEndedSessions(repo, sessions); stale >= staleEndedSessionWarnThreshold {
		warnStaleEndedSessions(ctx, stale)
	}

	return nil
}

// postCommitProcessSession handles a single session within the PostCommit loop.
// Pre-resolved git objects (headTree, parentTree) are shared across all sessions;
// per-session shadow ref/tree are resolved once here and threaded through sub-calls.
func (s *ManualCommitStrategy) postCommitProcessSession(
	ctx context.Context,
	repo *git.Repository,
	state *SessionState,
	transitionCtx *session.TransitionContext,
	checkpointID id.CheckpointID,
	head *plumbing.Reference,
	commit *object.Commit,
	newHead string,
	repoDir string,
	headTree, parentTree *object.Tree,
	committedFileSet map[string]struct{},
	shadowBranchesToDelete map[string]struct{},
	uncondensedActiveOnBranch map[string]bool,
) {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)

	// Pre-resolve shadow branch ref and tree for this session.
	// These are read 4+ times across sessionHasNewContent, filesOverlapWithContent,
	// CondenseSession, filesWithRemainingAgentChanges, and calculateSessionAttributions.
	_, resolveShadowBranchSpan := perf.Start(ctx, "resolve_shadow_branch")
	var shadowRef *plumbing.Reference
	var shadowTree *object.Tree
	if ref, refErr := repo.Reference(plumbing.NewBranchReferenceName(shadowBranchName), true); refErr == nil {
		shadowRef = ref
		if sc, scErr := repo.CommitObject(ref.Hash()); scErr == nil {
			if st, stErr := sc.Tree(); stErr == nil {
				shadowTree = st
			}
		}
	}
	resolveShadowBranchSpan.End()

	// Check for new content (needed for TransitionContext and condensation).
	// Fail-open: if content check errors, assume new content exists so we
	// don't silently skip data that should have been condensed.
	//
	// For ACTIVE sessions: the commit has a checkpoint trailer (verified above),
	// meaning PrepareCommitMsg already determined this commit is session-related.
	// The trailer is only added when either:
	//   - No TTY (agent/subagent committing) — added unconditionally
	//   - TTY (human committing) — added after content detection confirmed agent work
	// In both cases, PrepareCommitMsg already validated this commit. We trust
	// that decision here. Transcript-based re-validation is unreliable because
	// subagent transcripts may not be available yet (subagent still running).
	_, checkContentSpan := perf.Start(ctx, "check_session_content")
	var hasNew bool
	if state.Phase.IsActive() {
		hasNew = true
	} else {
		var contentErr error
		hasNew, contentErr = s.sessionHasNewContent(ctx, repo, state, contentCheckOpts{shadowTree: shadowTree})
		if contentErr != nil {
			hasNew = true
			logging.Debug(logCtx, "post-commit: error checking session content, assuming new content",
				slog.String("session_id", state.SessionID),
				slog.String("error", contentErr.Error()),
			)
		}
	}
	transitionCtx.HasFilesTouched = len(state.FilesTouched) > 0

	// Save FilesTouched BEFORE TransitionAndLog — the handler's condensation
	// clears it, but we need the original list for carry-forward computation.
	// Only fall back to transcript extraction for ACTIVE sessions — IDLE/ENDED
	// sessions have FilesTouched already populated by SaveStep/mergeFilesTouched.
	var filesTouchedBefore []string
	if state.Phase.IsActive() {
		filesTouchedBefore = s.resolveFilesTouched(ctx, state)
	} else if len(state.FilesTouched) > 0 {
		filesTouchedBefore = make([]string, len(state.FilesTouched))
		copy(filesTouchedBefore, state.FilesTouched)
	}
	checkContentSpan.End()

	logging.Debug(logCtx, "post-commit: carry-forward prep",
		slog.String("session_id", state.SessionID),
		slog.Bool("is_active", state.Phase.IsActive()),
		slog.String("transcript_path", state.TranscriptPath),
		slog.Int("files_touched_before", len(filesTouchedBefore)),
		slog.Any("files", filesTouchedBefore),
	)

	// Run the state machine transition with handler for strategy-specific actions.
	_, transitionAndCondenseSpan := perf.Start(ctx, "transition_and_condense")
	handler := &postCommitActionHandler{
		s:                      s,
		ctx:                    ctx,
		repo:                   repo,
		checkpointID:           checkpointID,
		head:                   head,
		commit:                 commit,
		newHead:                newHead,
		repoDir:                repoDir,
		shadowBranchName:       shadowBranchName,
		shadowBranchesToDelete: shadowBranchesToDelete,
		committedFileSet:       committedFileSet,
		hasNew:                 hasNew,
		filesTouchedBefore:     filesTouchedBefore,
		headTree:               headTree,
		parentTree:             parentTree,
		shadowRef:              shadowRef,
		shadowTree:             shadowTree,
	}

	if err := TransitionAndLog(ctx, state, session.EventGitCommit, *transitionCtx, handler); err != nil {
		logging.Warn(logCtx, "post-commit action handler error",
			slog.String("error", err.Error()))
	}
	transitionAndCondenseSpan.End()

	// Record checkpoint ID for ACTIVE sessions so HandleTurnEnd can finalize
	// with full transcript. IDLE/ENDED sessions already have complete transcripts.
	// NOTE: This check runs AFTER TransitionAndLog updated the phase. It relies on
	// ACTIVE + GitCommit → ACTIVE (phase stays ACTIVE). If that state machine
	// transition ever changed, this guard would silently stop recording IDs.
	if handler.condensed && state.Phase.IsActive() {
		state.TurnCheckpointIDs = append(state.TurnCheckpointIDs, checkpointID.String())
	}

	// Carry forward remaining uncommitted files so the next commit gets its
	// own checkpoint ID. This applies to ALL phases — if a user splits their
	// commit across two `git commit` invocations, each gets a 1:1 checkpoint.
	// Uses content-aware comparison: if user did `git add -p` and committed
	// partial changes, the file still has remaining agent changes to carry forward.
	_, carryForwardSpan := perf.Start(ctx, "carry_forward_files")
	if handler.condensed {
		remainingFiles := filesWithRemainingAgentChanges(ctx, repo, shadowBranchName, commit, filesTouchedBefore, committedFileSet, overlapOpts{
			headTree:   headTree,
			shadowTree: shadowTree,
		})
		state.FilesTouched = remainingFiles
		logging.Debug(logCtx, "post-commit: carry-forward decision (content-aware)",
			slog.String("session_id", state.SessionID),
			slog.Int("files_touched_before", len(filesTouchedBefore)),
			slog.Int("committed_files", len(committedFileSet)),
			slog.Int("remaining_files", len(remainingFiles)),
			slog.Any("remaining", remainingFiles),
			slog.Any("committed_files", committedFileSet),
		)
		if len(remainingFiles) > 0 {
			s.carryForwardToNewShadowBranch(ctx, repo, state, remainingFiles)
		}

		// Clear filesystem prompt.txt only when ALL files are committed.
		// If carry-forward files remain, the prompt must persist so the next
		// condensation (triggered by the next commit) can read it.
		if len(remainingFiles) == 0 {
			clearFilesystemPrompt(ctx, state.SessionID)
		}
	}
	carryForwardSpan.End()

	// Mark ENDED sessions as fully condensed when no carry-forward remains.
	// PostCommit will skip these sessions entirely on future commits.
	// They persist only for LastCheckpointID (amend trailer restoration).
	if handler.condensed && state.Phase == session.PhaseEnded && len(state.FilesTouched) == 0 {
		state.FullyCondensed = true
	}

	// Save the updated state
	_, saveSessionStateSpan := perf.Start(ctx, "save_session_state")
	if err := s.saveSessionState(ctx, state); err != nil {
		logging.Warn(logCtx, "failed to update session state",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()))
	}
	saveSessionStateSpan.End()

	// Only preserve shadow branch for active sessions that were NOT condensed.
	// Condensed sessions already have their data on entire/checkpoints/v1.
	if state.Phase.IsActive() && !handler.condensed {
		uncondensedActiveOnBranch[shadowBranchName] = true
	}
}

// condenseAndUpdateState runs condensation for a session and updates state afterward.
// Returns true if condensation succeeded.
func (s *ManualCommitStrategy) condenseAndUpdateState(
	ctx context.Context,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	state *SessionState,
	head *plumbing.Reference,
	shadowBranchName string,
	shadowBranchesToDelete map[string]struct{},
	committedFiles map[string]struct{},
	opts ...condenseOpts,
) bool {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	result, err := s.CondenseSession(ctx, repo, checkpointID, state, committedFiles, opts...)
	if err != nil {
		logging.Warn(logCtx, "condensation failed",
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
	state.CheckpointTranscriptSize = int64(len(result.Transcript))

	// Clear attribution tracking — condensation already used these values
	state.PromptAttributions = nil
	state.PendingPromptAttribution = nil
	state.FilesTouched = nil

	// NOTE: filesystem prompt.txt is NOT cleared here. The caller (PostCommit handler)
	// decides whether to clear it based on carry-forward: if remaining files exist,
	// the prompt must persist so the next condensation can read it.

	// Save checkpoint ID so subsequent commits can reuse it (e.g., amend restores trailer)
	state.LastCheckpointID = checkpointID

	logging.Info(logCtx, "session condensed",
		slog.String("strategy", "manual-commit"),
		slog.String("session_id", state.SessionID),
		slog.String("checkpoint_id", result.CheckpointID.String()),
		slog.Int("checkpoints_condensed", result.CheckpointsCount),
		slog.Int("transcript_lines", result.TotalTranscriptLines),
	)

	return true
}

// updateBaseCommitIfChanged updates BaseCommit to newHead if it changed.
// Only updates ACTIVE sessions. IDLE/ENDED sessions should NOT have their
// BaseCommit updated, as this would cause them to be incorrectly associated
// with a new shadow branch and potentially condensed on future commits.
func (s *ManualCommitStrategy) updateBaseCommitIfChanged(ctx context.Context, state *SessionState, newHead string) {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	// Only update ACTIVE sessions. IDLE/ENDED sessions are kept around for
	// LastCheckpointID reuse and should not be advanced to HEAD.
	if !state.Phase.IsActive() {
		logging.Debug(logCtx, "post-commit: updateBaseCommitIfChanged skipped non-active session",
			slog.String("session_id", state.SessionID),
			slog.String("phase", string(state.Phase)),
		)
		return
	}
	if state.BaseCommit != newHead {
		state.BaseCommit = newHead
		// Keep AttributionBaseCommit in sync to prevent stale base drift.
		// Without this, a subsequent condensation would diff from the old base,
		// inflating human_added with lines from unrelated prior commits.
		state.AttributionBaseCommit = newHead
		logging.Debug(logCtx, "post-commit: updated BaseCommit and AttributionBaseCommit",
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
func (s *ManualCommitStrategy) postCommitUpdateBaseCommitOnly(ctx context.Context, head *plumbing.Reference) {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return // Silent failure — hooks must be resilient
	}

	sessions, err := s.findSessionsForWorktree(ctx, worktreePath)
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
			logging.Debug(logCtx, "post-commit (no trailer): updating BaseCommit and AttributionBaseCommit",
				slog.String("session_id", state.SessionID),
				slog.String("old_base", truncateHash(state.BaseCommit)),
				slog.String("new_head", truncateHash(newHead)),
			)
			state.BaseCommit = newHead
			// Keep AttributionBaseCommit in sync to prevent stale base drift.
			// Without this, a subsequent condensation would diff from the old base,
			// inflating human_added with lines from unrelated prior commits.
			state.AttributionBaseCommit = newHead
			if err := s.saveSessionState(ctx, state); err != nil {
				logging.Warn(logCtx, "failed to update session state",
					slog.String("session_id", state.SessionID),
					slog.String("error", err.Error()))
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
// Computes the staged files list once and reuses it across all sessions to avoid
// redundant `git diff --cached` calls (previously called up to 3 times per session).
func (s *ManualCommitStrategy) filterSessionsWithNewContent(ctx context.Context, repo *git.Repository, sessions []*SessionState) []*SessionState {
	logCtx := logging.WithComponent(ctx, "manual-commit")
	var result []*SessionState

	// Compute staged files once for all sessions.
	// On error, pass nil — sessionHasNewContent treats nil stagedFiles as
	// "unavailable" and skips overlap checks, falling through to other heuristics.
	stagedFiles, err := getStagedFiles(ctx)
	if err != nil {
		logging.Debug(logCtx,
			"filterSessionsWithNewContent: getStagedFiles failed, skipping overlap checks",
			slog.String("error", err.Error()),
		)
		stagedFiles = nil
	}

	for _, state := range sessions {
		// Skip fully-condensed ended sessions — no new content possible.
		if state.FullyCondensed && state.Phase == session.PhaseEnded {
			logging.Debug(logCtx, "filterSessionsWithNewContent: skipping fully-condensed ended session",
				slog.String("session_id", state.SessionID),
			)
			continue
		}
		hasNew, err := s.sessionHasNewContent(ctx, repo, state, contentCheckOpts{stagedFiles: stagedFiles})
		if err != nil {
			logging.Debug(logCtx, "filterSessionsWithNewContent: error checking session, including it (fail-open)",
				slog.String("session_id", state.SessionID),
				slog.String("error", err.Error()),
			)
			// On error, include the session (fail open for hooks)
			result = append(result, state)
			continue
		}
		if !hasNew {
			logging.Debug(logCtx, "filterSessionsWithNewContent: session has no new content",
				slog.String("session_id", state.SessionID),
				slog.String("phase", string(state.Phase)),
				slog.Int("files_touched", len(state.FilesTouched)),
			)
		}
		if hasNew {
			result = append(result, state)
		}
	}

	return result
}

// contentCheckOpts holds pre-computed values for sessionHasNewContent to avoid
// redundant work across multiple sessions in a single hook invocation.
type contentCheckOpts struct {
	// stagedFiles is the pre-computed list of staged files (from getStagedFiles).
	// nil means staged files are unavailable (error or PostCommit context where
	// files are already committed) — callers skip overlap checks and fall through
	// to other heuristics (e.g., transcript growth).
	// Non-nil empty means successfully resolved but no files are staged.
	stagedFiles []string

	// shadowTree, when non-nil, is used directly to avoid redundant shadow branch
	// resolution (the shadow ref/commit/tree were already resolved by the caller).
	shadowTree *object.Tree
}

// sessionHasNewContent checks if a session has new transcript content
// beyond what was already condensed.
// The opts parameter provides pre-computed values to avoid redundant work.
func (s *ManualCommitStrategy) sessionHasNewContent(ctx context.Context, repo *git.Repository, state *SessionState, opts contentCheckOpts) (bool, error) {
	logCtx := logging.WithComponent(ctx, "manual-commit")

	// Use cached shadow tree if provided
	var tree *object.Tree
	if opts.shadowTree != nil {
		tree = opts.shadowTree
	} else {
		// Resolve shadow branch from repo
		shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		refName := plumbing.NewBranchReferenceName(shadowBranchName)
		ref, err := repo.Reference(refName, true)
		if err != nil {
			logging.Debug(logCtx, "sessionHasNewContent: no shadow branch, checking live transcript",
				slog.String("session_id", state.SessionID),
				slog.String("shadow_branch", shadowBranchName),
			)
			return s.sessionHasNewContentFromLiveTranscript(ctx, state, opts.stagedFiles)
		}

		commit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return false, fmt.Errorf("failed to get commit object: %w", err)
		}

		tree, err = commit.Tree()
		if err != nil {
			return false, fmt.Errorf("failed to get commit tree: %w", err)
		}
	}

	// Look for transcript file — use blob size for fast growth check when possible.
	// This avoids reading the full transcript content (potentially tens of MB) just
	// to count lines, which was the main source of PostCommit latency with many sessions.
	metadataDir := paths.EntireMetadataDir + "/" + state.SessionID
	var hasTranscriptFile bool
	var transcriptBlobSize int64

	if size, sizeErr := tree.Size(metadataDir + "/" + paths.TranscriptFileName); sizeErr == nil {
		hasTranscriptFile = true
		transcriptBlobSize = size
	} else if size, sizeErr := tree.Size(metadataDir + "/" + paths.TranscriptFileNameLegacy); sizeErr == nil {
		hasTranscriptFile = true
		transcriptBlobSize = size
	}

	// If shadow branch exists but has no transcript (e.g., carry-forward from mid-session commit),
	// check if the session has FilesTouched. Carry-forward sets FilesTouched with remaining files.
	if !hasTranscriptFile {
		if len(state.FilesTouched) > 0 {
			// Shadow branch has files from carry-forward - check if staged files overlap
			// AND have matching content (content-aware check).
			if len(opts.stagedFiles) > 0 {
				// PrepareCommitMsg context: check staged files overlap with content
				result := stagedFilesOverlapWithContent(ctx, repo, tree, opts.stagedFiles, state.FilesTouched)
				logging.Debug(logCtx, "sessionHasNewContent: no transcript, carry-forward with staged files",
					slog.String("session_id", state.SessionID),
					slog.Int("files_touched", len(state.FilesTouched)),
					slog.Int("staged_files", len(opts.stagedFiles)),
					slog.Bool("result", result),
				)
				return result, nil
			}
			// PostCommit context: no staged files, but we have carry-forward files.
			// Return true and let the caller do the overlap check with committed files.
			logging.Debug(logCtx, "sessionHasNewContent: no transcript, carry-forward without staged files (post-commit context)",
				slog.String("session_id", state.SessionID),
				slog.Int("files_touched", len(state.FilesTouched)),
			)
			return true, nil
		}
		// No transcript and no FilesTouched - fall back to live transcript check
		logging.Debug(logCtx, "sessionHasNewContent: no transcript and no files touched, checking live transcript",
			slog.String("session_id", state.SessionID),
		)
		return s.sessionHasNewContentFromLiveTranscript(ctx, state, opts.stagedFiles)
	}

	// Check if there's new content to condense. Two cases:
	// 1. Transcript has grown since last condensation (new prompts/responses)
	// 2. FilesTouched has files not yet committed (carry-forward scenario)
	//
	// For PrepareCommitMsg context, we verify staged files overlap with session's files
	// using content-aware matching to detect reverted files.
	// For PostCommit context, stagedFiles is nil/empty (files already committed),
	// so we return true and let the caller do the overlap check via filesOverlapWithContent.

	// Fast path: compare blob size against stored size from last condensation.
	// This avoids reading the full transcript content just to count items.
	var hasTranscriptGrowth bool
	switch {
	case state.CheckpointTranscriptSize > 0:
		hasTranscriptGrowth = transcriptBlobSize > state.CheckpointTranscriptSize
	case state.CheckpointTranscriptStart > 0:
		// Legacy session: condensed at least once (has line count) but no size tracking.
		// Cannot safely compare sizes — conservatively assume growth so condensation
		// can do the full content check. After one condensation with the new CLI,
		// CheckpointTranscriptSize will be populated and this path won't be hit again.
		hasTranscriptGrowth = true
	default:
		// Never condensed (CheckpointTranscriptStart == 0): any content means growth.
		hasTranscriptGrowth = transcriptBlobSize > 0
	}
	hasUncommittedFiles := len(state.FilesTouched) > 0

	logging.Debug(logCtx, "sessionHasNewContent: transcript size check",
		slog.String("session_id", state.SessionID),
		slog.Int64("transcript_blob_size", transcriptBlobSize),
		slog.Int64("checkpoint_transcript_size", state.CheckpointTranscriptSize),
		slog.Bool("has_transcript_growth", hasTranscriptGrowth),
		slog.Bool("has_uncommitted_files", hasUncommittedFiles),
	)

	if !hasTranscriptGrowth && !hasUncommittedFiles {
		return false, nil // No new content and no carry-forward files
	}

	// Check if staged files overlap with session's files with content-aware matching.
	// This is primarily for PrepareCommitMsg; in PostCommit, stagedFiles is nil/empty.
	if len(opts.stagedFiles) > 0 {
		result := stagedFilesOverlapWithContent(ctx, repo, tree, opts.stagedFiles, state.FilesTouched)
		logging.Debug(logCtx, "sessionHasNewContent: staged files overlap check",
			slog.String("session_id", state.SessionID),
			slog.Int("staged_files", len(opts.stagedFiles)),
			slog.Bool("result", result),
		)
		return result, nil
	}

	// No staged files - either PostCommit context or edge case.
	//
	// For recently-active IDLE sessions, the staged set is already gone by
	// PostCommit, but carry-forward files from the just-ended turn must still
	// count as "new content". The caller performs the stricter committed-file
	// overlap check before actually condensing, which prevents false positives
	// from unrelated commits.
	//
	// Stale IDLE sessions and ENDED sessions should NOT take this path. Those
	// states may retain FilesTouched from older work, and treating that alone as
	// fresh content would incorrectly condense old sessions into unrelated commits.
	if hasUncommittedFiles && state.Phase == session.PhaseIdle && isRecentInteraction(state.LastInteractionTime) {
		logging.Debug(logCtx, "sessionHasNewContent: no staged files, returning true due to recent idle carry-forward files",
			slog.String("session_id", state.SessionID),
			slog.Bool("has_transcript_growth", hasTranscriptGrowth),
			slog.Bool("has_uncommitted_files", hasUncommittedFiles),
			slog.String("phase", string(state.Phase)),
		)
		return true, nil
	}

	// No staged files and no carry-forward files: transcript growth is the only
	// remaining signal that there is new session content to condense.
	logging.Debug(logCtx, "sessionHasNewContent: no staged files, returning transcript growth",
		slog.String("session_id", state.SessionID),
		slog.Bool("has_transcript_growth", hasTranscriptGrowth),
		slog.Bool("has_uncommitted_files", hasUncommittedFiles),
	)
	return hasTranscriptGrowth, nil
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
// stagedFiles is the pre-computed list of staged files from the caller.
//
// This handles the scenario where the agent commits mid-session before Stop.
func (s *ManualCommitStrategy) sessionHasNewContentFromLiveTranscript(ctx context.Context, state *SessionState, stagedFiles []string) (bool, error) {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	if !s.hasNewTranscriptWork(ctx, state) {
		return false, nil
	}

	// Prefer hook-populated files. If empty, extract from transcript directly —
	// hasNewTranscriptWork already called PrepareTranscript, so we bypass
	// resolveFilesTouched (which would prepare again) and extract directly.
	modifiedFiles := state.FilesTouched
	if len(modifiedFiles) == 0 {
		modifiedFiles = s.extractModifiedFilesFromLiveTranscript(ctx, state, state.CheckpointTranscriptStart)
	}
	if len(modifiedFiles) == 0 {
		return false, nil
	}

	logging.Debug(logCtx, "live transcript check: found file modifications",
		slog.String("session_id", state.SessionID),
		slog.Int("modified_files", len(modifiedFiles)),
	)

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

// resolveFilesTouched returns the file list for a session.
// Prefers hook-populated state.FilesTouched, falls back to transcript extraction.
// All call sites that need "what files did the agent touch?" should use this.
//
// Handles PrepareTranscript internally before falling back to extraction,
// so callers don't need to prepare the transcript first.
func (s *ManualCommitStrategy) resolveFilesTouched(ctx context.Context, state *SessionState) []string {
	if len(state.FilesTouched) > 0 {
		result := make([]string, len(state.FilesTouched))
		copy(result, state.FilesTouched)
		return result
	}

	// Prepare transcript before extraction (e.g., OpenCode `opencode export`).
	prepareTranscriptForState(ctx, state)

	return s.extractModifiedFilesFromLiveTranscript(ctx, state, state.CheckpointTranscriptStart)
}

// hasNewTranscriptWork checks if the agent has done work since the last condensation.
// Uses agent-delegated GetTranscriptPosition() — does NOT do file extraction.
// All call sites that need "has the agent done new work?" should use this.
//
// Returns false if: no transcript path, unknown agent type, agent doesn't implement
// TranscriptAnalyzer, or GetTranscriptPosition fails. This is intentional fail-safe
// behavior: callers treat false as "no new work detected", which conservatively
// skips condensation on errors.
func (s *ManualCommitStrategy) hasNewTranscriptWork(ctx context.Context, state *SessionState) bool {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	if state.TranscriptPath == "" || state.AgentType == "" {
		return false
	}

	// Re-resolve transcript path — handles agents that relocate transcripts mid-session.
	if _, resolveErr := resolveTranscriptPath(state); resolveErr != nil {
		logging.Debug(logCtx, "hasNewTranscriptWork: transcript path resolution failed",
			slog.String("session_id", state.SessionID),
			slog.Any("error", resolveErr),
		)
		return false
	}

	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		return false
	}

	// Ensure transcript file is up-to-date (OpenCode creates/refreshes it via `opencode export`).
	// Only wait for flush when the session is active — for idle/ended sessions the
	// transcript is already fully flushed (the Stop hook completed the flush).
	if state.Phase.IsActive() {
		if preparer, ok := agent.AsTranscriptPreparer(ag); ok {
			if prepErr := preparer.PrepareTranscript(ctx, state.TranscriptPath); prepErr != nil {
				logging.Debug(logCtx, "prepare transcript failed",
					slog.String("session_id", state.SessionID),
					slog.String("agent_type", string(state.AgentType)),
					slog.String("transcript_path", state.TranscriptPath),
					slog.Any("error", prepErr),
				)
			}
		}
	}
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	if !ok {
		return false
	}

	currentPos, err := analyzer.GetTranscriptPosition(state.TranscriptPath)
	if err != nil {
		logging.Debug(logCtx, "hasNewTranscriptWork: GetTranscriptPosition failed",
			slog.String("session_id", state.SessionID),
			slog.String("transcript_path", state.TranscriptPath),
			slog.Any("error", err),
		)
		return false
	}

	if currentPos <= state.CheckpointTranscriptStart {
		logging.Debug(logCtx, "hasNewTranscriptWork: no new content",
			slog.String("session_id", state.SessionID),
			slog.Int("current_pos", currentPos),
			slog.Int("start_offset", state.CheckpointTranscriptStart),
		)
		return false
	}

	return true
}

// extractModifiedFilesFromLiveTranscript extracts modified files from the live transcript
// (including subagent transcripts) starting from the given offset, and normalizes them
// to repo-relative paths. Returns the normalized file list.
//
// Callers must ensure the transcript is prepared (e.g., via prepareTranscriptForState
// or hasNewTranscriptWork) before calling this function.
func (s *ManualCommitStrategy) extractModifiedFilesFromLiveTranscript(ctx context.Context, state *SessionState, offset int) []string {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	if state.TranscriptPath == "" || state.AgentType == "" {
		return nil
	}

	// Re-resolve transcript path — handles agents that relocate transcripts mid-session.
	if _, resolveErr := resolveTranscriptPath(state); resolveErr != nil {
		logging.Debug(logCtx, "extractModifiedFilesFromLiveTranscript: transcript path resolution failed",
			slog.String("session_id", state.SessionID),
			slog.Any("error", resolveErr),
		)
		return nil
	}

	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		return nil
	}

	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	if !ok {
		return nil
	}

	var modifiedFiles []string

	// For Claude Code, use ExtractAllModifiedFiles which parses the main transcript
	// AND subagent transcripts in a single pass, avoiding redundant parsing.
	if state.AgentType == agent.AgentTypeClaudeCode {
		subagentsDir := filepath.Join(filepath.Dir(state.TranscriptPath), state.SessionID, "subagents")
		transcriptData, readErr := os.ReadFile(state.TranscriptPath)
		if readErr != nil {
			logging.Debug(logCtx, "extractModifiedFilesFromLiveTranscript: failed to read transcript",
				slog.String("session_id", state.SessionID),
				slog.String("error", readErr.Error()),
			)
		} else {
			// TODO: fix when we refactor this area.
			// rather than instantiating claude specifically, we should iterate agents.
			c := &claudecode.ClaudeCodeAgent{}
			allFiles, extractErr := c.ExtractAllModifiedFiles(transcriptData, offset, subagentsDir)
			if extractErr != nil {
				logging.Debug(logCtx, "extractModifiedFilesFromLiveTranscript: extraction failed",
					slog.String("session_id", state.SessionID),
					slog.String("error", extractErr.Error()),
				)
			} else {
				modifiedFiles = allFiles
			}
		}
	} else {
		files, _, err := analyzer.ExtractModifiedFilesFromOffset(state.TranscriptPath, offset)
		if err != nil {
			logging.Debug(logCtx, "extractModifiedFilesFromLiveTranscript: main transcript extraction failed",
				slog.String("transcript_path", state.TranscriptPath),
				slog.Any("error", err),
			)
		} else {
			modifiedFiles = files
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
		if wp, wpErr := paths.WorktreeRoot(ctx); wpErr == nil {
			basePath = wp
		}
	}
	if basePath != "" {
		normalized := make([]string, 0, len(modifiedFiles))
		for _, f := range modifiedFiles {
			if rel := paths.ToRelativePath(f, basePath); rel != "" {
				normalized = append(normalized, filepath.ToSlash(rel))
			} else {
				normalized = append(normalized, filepath.ToSlash(f))
			}
		}
		modifiedFiles = normalized
	}

	return modifiedFiles
}

// tryAgentCommitFastPath skips content detection for mid-turn agent commits.
// Returns true if the fast path was taken (trailer added or attempt made),
// false if the caller should continue with normal content detection.
//
// The fast path activates when an ACTIVE session exists and either:
//   - No TTY is available (agent subprocess, CI), or
//   - commit_linking="always" (user opted into auto-linking — needed because
//     some agents like Gemini subagents commit mid-turn from processes that
//     have /dev/tty but can't respond to prompts, and content detection fails
//     since the shadow branch doesn't exist yet).
func (s *ManualCommitStrategy) tryAgentCommitFastPath(ctx context.Context, commitMsgFile string, sessions []*SessionState, source string) bool {
	noTTY := !hasTTY()
	skipContentDetection := noTTY
	if !skipContentDetection {
		if stngs, err := settings.Load(ctx); err == nil {
			skipContentDetection = stngs.GetCommitLinking() == settings.CommitLinkingAlways
		}
	}
	if !skipContentDetection {
		return false
	}
	logCtx := logging.WithComponent(ctx, "checkpoint")
	for _, state := range sessions {
		if state.Phase.IsActive() {
			_ = s.addTrailerForAgentCommit(logCtx, commitMsgFile, state, source) //nolint:errcheck // always returns nil; kept for signature stability
			return true
		}
	}
	// Log why fast path didn't fire — collect session phases for diagnostics.
	phases := make([]string, 0, len(sessions))
	for _, state := range sessions {
		phases = append(phases, string(state.Phase))
	}
	logging.Debug(logCtx, "prepare-commit-msg: fast path found no ACTIVE sessions",
		slog.Bool("no_tty", noTTY),
		slog.Int("sessions", len(sessions)),
		slog.Any("session_phases", phases),
	)
	return false
}

// addTrailerForAgentCommit handles the fast path when an agent is committing
// (ACTIVE session + no TTY). Generates a checkpoint ID and adds the trailer
// directly, bypassing content detection and interactive prompts.
func (s *ManualCommitStrategy) addTrailerForAgentCommit(logCtx context.Context, commitMsgFile string, state *SessionState, source string) error { //nolint:unparam // kept for signature stability
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

	if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil { //nolint:gosec // path from git hook arg
		return nil //nolint:nilerr // Hook must be silent on failure
	}
	return nil
}

// addCheckpointTrailer adds the Entire-Checkpoint trailer to a commit message.
// Delegates to trailers.AppendCheckpointTrailer for trailer-aware formatting.
func addCheckpointTrailer(message string, checkpointID id.CheckpointID) string {
	return trailers.AppendCheckpointTrailer(message, checkpointID.String())
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
// userPrompt is the user's prompt text (stored truncated as LastPrompt for display).
// model is the LLM model identifier (e.g., "claude-sonnet-4-20250514"); empty if unknown.
func (s *ManualCommitStrategy) InitializeSession(ctx context.Context, sessionID string, agentType types.AgentType, transcriptPath string, userPrompt string, model string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Check if session already exists
	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to check session state: %w", err)
	}

	if state != nil && state.BaseCommit != "" {
		// Session is fully initialized — apply phase transition for TurnStart.
		if transErr := TransitionAndLog(ctx, state, session.EventTurnStart, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			logging.Warn(logging.WithComponent(ctx, "hooks"), "turn start transition failed",
				slog.String("session_id", sessionID),
				slog.String("error", transErr.Error()))
		}

		// Generate a new TurnID for each turn (correlates carry-forward checkpoints)
		turnID, err := id.Generate()
		if err != nil {
			return fmt.Errorf("failed to generate turn ID: %w", err)
		}
		state.TurnID = turnID.String()

		// Set AgentType from hook context if not yet set
		if state.AgentType == "" && agentType != "" {
			state.AgentType = agentType
		}

		// Update ModelName if provided (model can change between turns)
		if model != "" {
			state.ModelName = model
		}

		// Update LastPrompt on every turn so condensation always has the current prompt
		if userPrompt != "" {
			state.LastPrompt = truncatePromptForStorage(userPrompt)
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
		promptAttr := s.calculatePromptAttributionAtStart(ctx, repo, state)
		state.PendingPromptAttribution = &promptAttr

		// Check if HEAD has moved (user pulled/rebased or committed)
		// migrateShadowBranchIfNeeded handles renaming the shadow branch and updating state.BaseCommit
		if _, err := s.migrateShadowBranchIfNeeded(ctx, repo, state); err != nil {
			return fmt.Errorf("failed to check/migrate shadow branch: %w", err)
		}

		if err := s.saveSessionState(ctx, state); err != nil {
			return fmt.Errorf("failed to update session state: %w", err)
		}
		return nil
	}
	// If state exists but BaseCommit is empty, it's a partial state from concurrent warning
	// Continue below to properly initialize it

	// Initialize new session
	state, err = s.initializeSession(ctx, repo, sessionID, agentType, transcriptPath, userPrompt, model)
	if err != nil {
		return fmt.Errorf("failed to initialize session: %w", err)
	}

	// Apply phase transition: new session starts as ACTIVE.
	if transErr := TransitionAndLog(ctx, state, session.EventTurnStart, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
		logging.Warn(logging.WithComponent(ctx, "hooks"), "turn start transition failed",
			slog.String("session_id", sessionID),
			slog.String("error", transErr.Error()))
	}

	// Calculate attribution for pre-prompt edits
	// This captures any user edits made before the first prompt
	promptAttr := s.calculatePromptAttributionAtStart(ctx, repo, state)
	state.PendingPromptAttribution = &promptAttr
	if err = s.saveSessionState(ctx, state); err != nil {
		return fmt.Errorf("failed to save attribution: %w", err)
	}

	logging.Info(logging.WithComponent(ctx, "hooks"), "initialized shadow session",
		slog.String("session_id", sessionID))
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
	ctx context.Context,
	repo *git.Repository,
	state *SessionState,
) PromptAttribution {
	logCtx := logging.WithComponent(ctx, "attribution")
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

// getStagedFiles returns a list of files staged for commit using native git CLI.
// This is much faster than go-git's worktree.Status() which scans the entire
// working tree. `git diff --cached --name-only` uses native git's optimized index
// and filesystem monitors.
//
// Returns (non-nil empty slice, nil) when no files are staged — callers can
// distinguish "no staged files" from "error resolving staged files" (nil, err).
func getStagedFiles(ctx context.Context) ([]string, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --cached: %w", err)
	}

	staged := []string{}
	trimmed := strings.TrimSpace(string(output))
	// Normalize Windows line endings (\r\n) to Unix (\n) for cross-platform git output
	trimmed = strings.ReplaceAll(trimmed, "\r\n", "\n")
	for _, line := range strings.Split(trimmed, "\n") {
		if line != "" {
			staged = append(staged, filepath.ToSlash(line))
		}
	}
	return staged, nil
}

// getLastPrompt retrieves the most recent user prompt from a session's shadow branch.
// Reads prompt.txt directly from the shadow branch tree instead of parsing the full
// transcript (which involves token counting, context generation, etc.).
// Returns empty string if no prompt can be retrieved.
func (s *ManualCommitStrategy) getLastPrompt(_ context.Context, repo *git.Repository, state *SessionState) string {
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return ""
	}

	tree, err := commit.Tree()
	if err != nil {
		return ""
	}

	// Read prompt.txt directly from the shadow branch tree.
	// Prompts are separated by "\n\n---\n\n" — extract the last one.
	metadataDir := paths.EntireMetadataDir + "/" + state.SessionID
	promptPath := metadataDir + "/" + paths.PromptFileName
	file, err := tree.File(promptPath)
	if err != nil {
		return ""
	}

	content, err := file.Contents()
	if err != nil {
		return ""
	}

	return extractLastPrompt(content)
}

// extractLastPrompt returns the last non-empty prompt from prompt.txt content.
// Prompts are separated by "\n\n---\n\n".
func extractLastPrompt(content string) string {
	if content == "" {
		return ""
	}

	prompts := strings.Split(content, "\n\n---\n\n")
	// Iterate backwards to find the last non-empty prompt
	for i := len(prompts) - 1; i >= 0; i-- {
		cleaned := strings.TrimSpace(prompts[i])
		if cleaned != "" && !isOnlySeparators(cleaned) {
			return cleaned
		}
	}
	return ""
}

// TODO: check if its duplicated
// readPromptsFromShadowBranch reads prompt.txt from the shadow branch tree.
// Returns all prompts split on "\n\n---\n\n", or nil if prompt.txt is not available.
func readPromptsFromShadowBranch(_ context.Context, repo *git.Repository, state *SessionState) []string {
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil
	}

	metadataDir := paths.EntireMetadataDir + "/" + state.SessionID
	promptPath := metadataDir + "/" + paths.PromptFileName
	file, err := tree.File(promptPath)
	if err != nil {
		return nil
	}

	content, err := file.Contents()
	if err != nil {
		return nil
	}

	return splitPromptContent(content)
}

// HandleTurnEnd dispatches strategy-specific actions emitted when an agent turn ends.
// The primary job is to finalize all checkpoints from this turn with the full transcript.
//
// During a turn, PostCommit writes provisional transcript data (whatever was available
// at commit time). HandleTurnEnd replaces that with the complete session transcript
// (from prompt to stop event), ensuring every checkpoint has the full context.
//

func (s *ManualCommitStrategy) HandleTurnEnd(ctx context.Context, state *SessionState) error { //nolint:unparam // error return is part of the hook contract; callers check it
	// Finalize all checkpoints from this turn with the full transcript.
	//
	// IMPORTANT: This is best-effort - errors are logged but don't fail the hook.
	// Failing here would prevent session cleanup and could leave state inconsistent.
	// The provisional transcript from PostCommit is already persisted, so the
	// checkpoint isn't lost - it just won't have the complete transcript.
	errCount := s.finalizeAllTurnCheckpoints(ctx, state)
	if errCount > 0 {
		logCtx := logging.WithComponent(ctx, "checkpoint")
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
func (s *ManualCommitStrategy) finalizeAllTurnCheckpoints(ctx context.Context, state *SessionState) int {
	if len(state.TurnCheckpointIDs) == 0 {
		return 0 // No mid-turn commits to finalize
	}

	logCtx := logging.WithComponent(ctx, "checkpoint")

	logging.Info(logCtx, "finalizing turn checkpoints with full transcript",
		slog.String("session_id", state.SessionID),
		slog.Int("checkpoint_count", len(state.TurnCheckpointIDs)),
	)

	errCount := 0

	// Read full transcript from live transcript file, re-resolving the path if the
	// agent relocated it mid-session (e.g., Cursor CLI flat → nested layout change).
	if state.TranscriptPath == "" {
		logging.Warn(logCtx, "finalize: no transcript path, skipping",
			slog.String("session_id", state.SessionID),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}

	transcriptPath, resolveErr := resolveTranscriptPath(state)
	if resolveErr != nil {
		logging.Warn(logCtx, "finalize: transcript path resolution failed, skipping",
			slog.String("session_id", state.SessionID),
			slog.Any("error", resolveErr),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}

	fullTranscript, err := os.ReadFile(transcriptPath) //nolint:gosec // path validated by resolveTranscriptPath
	if err != nil || len(fullTranscript) == 0 {
		msg := "finalize: empty transcript, skipping"
		if err != nil {
			msg = "finalize: failed to read transcript, skipping"
		}
		logging.Warn(logCtx, msg,
			slog.String("session_id", state.SessionID),
			slog.String("transcript_path", state.TranscriptPath),
			slog.Any("error", err),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}

	// Open repository (needed for shadow branch prompt reading and checkpoint store)
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(logCtx, "finalize: failed to open repository",
			slog.String("error", err.Error()),
		)
		state.TurnCheckpointIDs = nil
		return 1 // Count as error - all checkpoints will be skipped
	}

	prompts := readPromptsFromShadowBranch(ctx, repo, state)
	if len(prompts) == 0 {
		prompts = readPromptsFromFilesystem(ctx, state.SessionID)
	}

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

	store := checkpoint.NewGitStore(repo)

	// Evaluate v2 flag once before the loop to avoid re-reading settings per checkpoint
	var v2Store *checkpoint.V2GitStore
	if settings.IsCheckpointsV2Enabled(logCtx) {
		v2Store = checkpoint.NewV2GitStore(repo)
	}

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

		updateOpts := checkpoint.UpdateCommittedOptions{
			CheckpointID: cpID,
			SessionID:    state.SessionID,
			Transcript:   fullTranscript,
			Prompts:      prompts,
			Agent:        state.AgentType,
		}

		// Generate compact transcript for v2 /main
		if v2Store != nil && len(fullTranscript) > 0 {
			finalAg, _ := agent.GetByAgentType(state.AgentType) //nolint:errcheck // ag may be nil for unknown agent types; compactTranscriptForV2 handles nil
			startLine := 0
			if content, readErr := store.ReadSessionContentByID(ctx, cpID, state.SessionID); readErr == nil && content != nil {
				startLine = content.Metadata.GetTranscriptStart()
			} else {
				errMsg := "unknown"
				if readErr != nil {
					errMsg = readErr.Error()
				}
				logging.Debug(logCtx, "finalize: failed to read checkpoint metadata, using full transcript for compact output",
					slog.String("checkpoint_id", cpIDStr),
					slog.String("session_id", state.SessionID),
					slog.String("error", errMsg),
				)
			}
			updateOpts.CompactTranscript = compactTranscriptForV2(logCtx, finalAg, fullTranscript, startLine)
		}

		updateErr := store.UpdateCommitted(ctx, updateOpts)
		if updateErr != nil {
			logging.Warn(logCtx, "finalize: failed to update checkpoint",
				slog.String("checkpoint_id", cpIDStr),
				slog.String("error", updateErr.Error()),
			)
			errCount++
			continue
		}

		// Dual-write: update v2 refs when enabled
		if v2Store != nil {
			if v2Err := v2Store.UpdateCommitted(logCtx, updateOpts); v2Err != nil {
				logging.Warn(logCtx, "v2 dual-write update failed",
					slog.String("checkpoint_id", cpIDStr),
					slog.String("error", v2Err.Error()),
				)
			}
		}

		logging.Info(logCtx, "finalize: checkpoint updated with full transcript",
			slog.String("checkpoint_id", cpIDStr),
			slog.String("session_id", state.SessionID),
		)
	}

	// Clear turn checkpoint IDs. Do NOT update CheckpointTranscriptStart here — it was
	// already set correctly by PostCommit: condenseAndUpdateState sets it to the total
	// transcript lines when condensing, and carryForwardToNewShadowBranch resets it to 0
	// when carry-forward is active. Overwriting here would break carry-forward by making
	// sessionHasNewContent think the transcript is fully consumed (no growth).
	state.TurnCheckpointIDs = nil

	return errCount
}

// filesChangedInCommit returns the set of files changed in a commit using git diff-tree.
// Uses the git CLI for faster performance vs go-git tree walks (lower constant factors).
// Falls back to go-git tree walk if git diff-tree fails, since an empty result would
// break downstream condensation and carry-forward logic.
func filesChangedInCommit(ctx context.Context, repoDir string, commit *object.Commit, headTree, parentTree *object.Tree) map[string]struct{} {
	var parentHash string
	if commit.NumParents() > 0 {
		parentHash = commit.ParentHashes[0].String()
	}
	result, err := gitops.DiffTreeFiles(ctx, repoDir, parentHash, commit.Hash.String())
	if err != nil {
		logging.Warn(ctx, "post-commit: git diff-tree failed, falling back to tree walk",
			slog.String("commit", commit.Hash.String()),
			slog.String("error", err.Error()),
		)
		return filesChangedInCommitFallback(ctx, headTree, parentTree)
	}
	return result
}

// filesChangedInCommitFallback uses go-git tree walks to compute changed files.
// Slower than git diff-tree but doesn't depend on an external process.
func filesChangedInCommitFallback(ctx context.Context, headTree, parentTree *object.Tree) map[string]struct{} {
	files, err := getAllChangedFilesBetweenTreesSlow(ctx, parentTree, headTree)
	if err != nil {
		logging.Warn(ctx, "post-commit: tree walk fallback also failed; condensation and carry-forward may be affected",
			slog.String("error", err.Error()),
		)
		return make(map[string]struct{})
	}
	result := make(map[string]struct{}, len(files))
	for _, f := range files {
		result[f] = struct{}{}
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
	ctx context.Context,
	repo *git.Repository,
	state *SessionState,
	remainingFiles []string,
) {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	store := checkpoint.NewGitStore(repo)

	// Don't include metadata directory in carry-forward. The carry-forward branch
	// only needs to preserve file content for comparison - not the transcript.
	// Including the transcript would cause sessionHasNewContent to always return true
	// because CheckpointTranscriptStart is reset to 0 for carry-forward.
	result, err := store.WriteTemporary(ctx, checkpoint.WriteTemporaryOptions{
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
	state.StepCount = 1
	state.CheckpointTranscriptStart = 0
	state.CheckpointTranscriptSize = 0
	state.LastCheckpointID = ""
	// NOTE: TurnCheckpointIDs is intentionally NOT cleared here. Those checkpoint
	// IDs from earlier in the turn still need finalization with the full transcript
	// when HandleTurnEnd runs at stop time.

	logging.Info(logCtx, "post-commit: carried forward remaining files",
		slog.String("session_id", state.SessionID),
		slog.Int("remaining_files", len(remainingFiles)),
	)
}
