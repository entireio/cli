package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

// wingmanLog writes a timestamped log line to stderr. In the detached subprocess,
// stderr is redirected to .entire/logs/wingman.log by the spawner.
func wingmanLog(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s [wingman] %s\n", time.Now().Format(time.RFC3339), msg)
}

const (
	// wingmanInitialDelay is how long to wait before starting the review,
	// letting the agent turn fully settle.
	wingmanInitialDelay = 10 * time.Second

	// wingmanReviewModel is the Claude model used for reviews.
	wingmanReviewModel = "sonnet"

	// wingmanGitTimeout is the timeout for git diff operations.
	wingmanGitTimeout = 60 * time.Second

	// wingmanReviewTimeout is the timeout for the claude --print review call.
	wingmanReviewTimeout = 5 * time.Minute

	// wingmanApplyTimeout is the timeout for the claude --continue auto-apply call.
	wingmanApplyTimeout = 15 * time.Minute
)

// wingmanCLIResponse represents the JSON response from the Claude CLI --output-format json.
type wingmanCLIResponse struct {
	Result string `json:"result"`
}

func newWingmanReviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__review",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runWingmanReview(args[0])
		},
	}
}

func runWingmanReview(payloadPath string) error {
	wingmanLog("review process started (pid=%d)", os.Getpid())
	wingmanLog("reading payload from %s", payloadPath)

	// Read payload from file (avoids OS argv limits with large payloads)
	payloadJSON, err := os.ReadFile(payloadPath) //nolint:gosec // path is from our own spawn
	if err != nil {
		wingmanLog("ERROR reading payload: %v", err)
		return fmt.Errorf("failed to read payload file: %w", err)
	}
	// Clean up payload file after reading
	_ = os.Remove(payloadPath)

	var payload WingmanPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		wingmanLog("ERROR unmarshaling payload: %v", err)
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	repoRoot := payload.RepoRoot
	if repoRoot == "" {
		wingmanLog("ERROR repo_root is empty in payload")
		return errors.New("repo_root is required in payload")
	}

	totalFiles := len(payload.ModifiedFiles) + len(payload.NewFiles) + len(payload.DeletedFiles)
	wingmanLog("session=%s repo=%s base_commit=%s files=%d",
		payload.SessionID, repoRoot, payload.BaseCommit, totalFiles)

	// Clean up lock file when review completes (regardless of success/failure)
	lockPath := filepath.Join(repoRoot, wingmanLockFile)
	defer func() {
		_ = os.Remove(lockPath)
		wingmanLog("lock file removed")
	}()

	// Initial delay: let the agent turn fully settle
	wingmanLog("waiting %s for agent turn to settle", wingmanInitialDelay)
	time.Sleep(wingmanInitialDelay)

	// Compute diff using the base commit captured at trigger time
	wingmanLog("computing diff against %s", payload.BaseCommit)
	diffStart := time.Now()
	diff, err := computeDiff(repoRoot, payload.BaseCommit)
	if err != nil {
		wingmanLog("ERROR computing diff: %v", err)
		return fmt.Errorf("failed to compute diff: %w", err)
	}

	if strings.TrimSpace(diff) == "" {
		wingmanLog("no changes found in diff, exiting")
		return nil // No changes to review
	}
	wingmanLog("diff computed: %d bytes in %s", len(diff), time.Since(diffStart).Round(time.Millisecond))

	// Build file list for the prompt
	allFiles := make([]string, 0, len(payload.ModifiedFiles)+len(payload.NewFiles)+len(payload.DeletedFiles))
	for _, f := range payload.ModifiedFiles {
		allFiles = append(allFiles, f+" (modified)")
	}
	for _, f := range payload.NewFiles {
		allFiles = append(allFiles, f+" (new)")
	}
	for _, f := range payload.DeletedFiles {
		allFiles = append(allFiles, f+" (deleted)")
	}
	fileList := strings.Join(allFiles, ", ")

	// Read session context from checkpoint data (best-effort)
	sessionContext := readSessionContext(repoRoot, payload.SessionID)
	if sessionContext != "" {
		wingmanLog("session context loaded: %d bytes", len(sessionContext))
	}

	// Build review prompt
	prompt := buildReviewPrompt(payload.Prompts, payload.CommitMessage, sessionContext, payload.SessionID, fileList, diff)
	wingmanLog("review prompt built: %d bytes", len(prompt))

	// Call Claude CLI for review
	wingmanLog("calling claude CLI (model=%s, timeout=%s)", wingmanReviewModel, wingmanReviewTimeout)
	reviewStart := time.Now()
	reviewText, err := callClaudeForReview(repoRoot, prompt)
	if err != nil {
		wingmanLog("ERROR claude CLI failed after %s: %v", time.Since(reviewStart).Round(time.Millisecond), err)
		return fmt.Errorf("failed to get review from Claude: %w", err)
	}
	wingmanLog("review received: %d bytes in %s", len(reviewText), time.Since(reviewStart).Round(time.Millisecond))

	// Write REVIEW.md
	reviewPath := filepath.Join(repoRoot, wingmanReviewFile)
	if err := os.MkdirAll(filepath.Dir(reviewPath), 0o750); err != nil {
		wingmanLog("ERROR creating directory: %v", err)
		return fmt.Errorf("failed to create directory: %w", err)
	}
	//nolint:gosec // G306: review file is not secrets
	if err := os.WriteFile(reviewPath, []byte(reviewText), 0o644); err != nil {
		wingmanLog("ERROR writing REVIEW.md: %v", err)
		return fmt.Errorf("failed to write REVIEW.md: %w", err)
	}
	wingmanLog("REVIEW.md written to %s", reviewPath)

	// Update dedup state — write directly to known path instead of using
	// os.Chdir (which mutates process-wide state).
	allFilePaths := make([]string, 0, len(payload.ModifiedFiles)+len(payload.NewFiles)+len(payload.DeletedFiles))
	allFilePaths = append(allFilePaths, payload.ModifiedFiles...)
	allFilePaths = append(allFilePaths, payload.NewFiles...)
	allFilePaths = append(allFilePaths, payload.DeletedFiles...)

	saveWingmanStateDirect(repoRoot, &WingmanState{
		SessionID:     payload.SessionID,
		FilesHash:     computeFilesHash(allFilePaths),
		ReviewedAt:    time.Now(),
		ReviewApplied: false,
	})
	wingmanLog("dedup state saved")

	// Check if session is idle right now (rare — user usually starts typing
	// during the 10s settle + review time). If idle, apply immediately.
	// Otherwise, the stop hook will handle auto-apply when the current turn ends.
	idle := isSessionIdle(payload.SessionID)
	wingmanLog("session idle check: idle=%v", idle)

	if !idle {
		wingmanLog("session is active, stop hook will handle auto-apply when turn ends")
		return nil
	}

	// Mark apply as attempted before triggering (retry prevention)
	now := time.Now()
	state := loadWingmanStateDirect(repoRoot)
	if state != nil {
		state.ApplyAttemptedAt = &now
		saveWingmanStateDirect(repoRoot, state)
	}

	wingmanLog("triggering auto-apply via claude --continue (session idle)")
	applyStart := time.Now()
	if err := triggerAutoApply(repoRoot); err != nil {
		wingmanLog("ERROR auto-apply failed after %s: %v", time.Since(applyStart).Round(time.Millisecond), err)
		return fmt.Errorf("failed to trigger auto-apply: %w", err)
	}
	wingmanLog("auto-apply completed in %s", time.Since(applyStart).Round(time.Millisecond))

	return nil
}

// computeDiff gets the full branch diff for review. It diffs the current HEAD
// against the merge base with the default branch (main/master), giving the
// reviewer a holistic view of all branch changes rather than just one commit.
func computeDiff(repoRoot string, _ string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wingmanGitTimeout)
	defer cancel()

	// Find the merge base with the default branch for a holistic branch diff.
	mergeBase := findMergeBase(ctx, repoRoot)
	if mergeBase != "" {
		wingmanLog("using merge-base %s for branch diff", mergeBase)
		diff, err := gitDiff(ctx, repoRoot, mergeBase)
		if err == nil && strings.TrimSpace(diff) != "" {
			return diff, nil
		}
		// Fall through to HEAD diff if merge-base diff fails or is empty
	}

	// Fallback: diff uncommitted changes against HEAD
	wingmanLog("falling back to HEAD diff")
	diff, err := gitDiff(ctx, repoRoot, "HEAD")
	if err != nil {
		return "", fmt.Errorf("git diff failed: %w", err)
	}

	// If no uncommitted changes, try the latest commit itself
	if strings.TrimSpace(diff) == "" {
		diff, err = gitDiff(ctx, repoRoot, "HEAD~1", "HEAD")
		if err != nil {
			return "", nil
		}
	}

	return diff, nil
}

// findMergeBase returns the merge-base commit between HEAD and the default
// branch (tries main, then master). Returns empty string if not found.
func findMergeBase(ctx context.Context, repoRoot string) string {
	for _, branch := range []string{"main", "master"} {
		cmd := exec.CommandContext(ctx, "git", "merge-base", branch, "HEAD") //nolint:gosec // branch is from hardcoded slice
		cmd.Dir = repoRoot
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

// gitDiff runs git diff with the given args and returns stdout.
func gitDiff(ctx context.Context, repoRoot string, args ...string) (string, error) {
	fullArgs := append([]string{"diff"}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...) //nolint:gosec // args are from internal logic
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git diff %v: %w (stderr: %s)", args, err, stderr.String())
	}
	return stdout.String(), nil
}

// callClaudeForReview calls the claude CLI to perform the review.
// repoRoot is the repository root so the reviewer can access the full codebase.
func callClaudeForReview(repoRoot, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wingmanReviewTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude",
		"--print",
		"--output-format", "json",
		"--model", wingmanReviewModel,
		"--setting-sources", "",
		// Grant read-only tool access so the reviewer can inspect source files
		// beyond just the diff. Permission bypass is safe here since tools are
		// restricted to read-only operations.
		"--allowedTools", "Read,Glob,Grep",
		"--permission-mode", "bypassPermissions",
	)

	// Run from repo root so the reviewer can read source files for context.
	// Loop-breaking is handled by --setting-sources "" (disables hooks) and
	// wingmanStripGitEnv (prevents git index pollution).
	cmd.Dir = repoRoot
	cmd.Env = wingmanStripGitEnv(os.Environ())

	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "", fmt.Errorf("claude CLI not found: %w", err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("claude CLI failed (exit %d): %s", exitErr.ExitCode(), stderr.String())
		}
		return "", fmt.Errorf("failed to run claude CLI: %w", err)
	}

	// Parse the CLI response
	var cliResponse wingmanCLIResponse
	if err := json.Unmarshal(stdout.Bytes(), &cliResponse); err != nil {
		return "", fmt.Errorf("failed to parse claude CLI response: %w", err)
	}

	return cliResponse.Result, nil
}

// readSessionContext reads the context.md file from the session's checkpoint
// metadata directory. Returns empty string if unavailable.
func readSessionContext(repoRoot, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	contextPath := filepath.Join(repoRoot, ".entire", "metadata", sessionID, "context.md")
	data, err := os.ReadFile(contextPath) //nolint:gosec // path constructed from repoRoot + session ID
	if err != nil {
		return ""
	}
	return string(data)
}

// isSessionIdle checks if the given session is in the IDLE phase.
func isSessionIdle(sessionID string) bool {
	state, err := strategy.LoadSessionState(sessionID)
	if err != nil || state == nil {
		return false
	}
	return state.Phase == session.PhaseIdle
}

// runWingmanApply is the entrypoint for the __apply subcommand, spawned by the
// stop hook when a pending REVIEW.md is detected. It re-checks preconditions
// and triggers claude --continue to apply the review.
func runWingmanApply(repoRoot string) error {
	wingmanLog("apply process started (pid=%d)", os.Getpid())

	reviewPath := filepath.Join(repoRoot, wingmanReviewFile)
	if !fileExists(reviewPath) {
		wingmanLog("no REVIEW.md found, nothing to apply")
		return nil
	}

	// Retry prevention: check if apply was already attempted for this review
	state := loadWingmanStateDirect(repoRoot)
	if state != nil && state.ApplyAttemptedAt != nil {
		wingmanLog("apply already attempted at %s, skipping", state.ApplyAttemptedAt.Format(time.RFC3339))
		return nil
	}

	// Re-check session is still idle (user may have typed during spawn delay)
	if state != nil && state.SessionID != "" {
		if !isSessionIdle(state.SessionID) {
			wingmanLog("session became active during spawn, aborting (next stop hook will retry)")
			return nil
		}
	}

	// Mark apply as attempted BEFORE triggering
	if state != nil {
		now := time.Now()
		state.ApplyAttemptedAt = &now
		saveWingmanStateDirect(repoRoot, state)
	}

	wingmanLog("triggering auto-apply via claude --continue")
	applyStart := time.Now()
	if err := triggerAutoApply(repoRoot); err != nil {
		wingmanLog("ERROR auto-apply failed after %s: %v", time.Since(applyStart).Round(time.Millisecond), err)
		return fmt.Errorf("failed to trigger auto-apply: %w", err)
	}
	wingmanLog("auto-apply completed in %s", time.Since(applyStart).Round(time.Millisecond))

	return nil
}

// triggerAutoApply spawns claude --continue to apply the review suggestions.
func triggerAutoApply(repoRoot string) error {
	ctx, cancel := context.WithTimeout(context.Background(), wingmanApplyTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude",
		"--continue",
		"--print",
		"--setting-sources", "",
		// Auto-accept edits so the agent can modify files and delete REVIEW.md
		// without requiring user consent (this runs in a background process).
		"--permission-mode", "acceptEdits",
		wingmanApplyInstruction,
	)
	cmd.Dir = repoRoot
	// Strip GIT_* env vars to prevent hook interference, and set
	// ENTIRE_WINGMAN_APPLY=1 so git hooks (post-commit) know not to
	// trigger another wingman review (prevents infinite recursion).
	env := wingmanStripGitEnv(os.Environ())
	env = append(env, "ENTIRE_WINGMAN_APPLY=1")
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	return cmd.Run() //nolint:wrapcheck // caller wraps
}

// wingmanStripGitEnv returns a copy of env with all GIT_* variables removed.
// This prevents a subprocess from discovering or modifying the parent's git repo
// when we want isolation (e.g., running claude --print for review).
func wingmanStripGitEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "GIT_") {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// saveWingmanStateDirect writes the wingman state file directly to a known path
// under repoRoot, avoiding os.Chdir (which mutates process-wide state).
func saveWingmanStateDirect(repoRoot string, state *WingmanState) {
	statePath := filepath.Join(repoRoot, wingmanStateFile)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o750); err != nil {
		return
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	//nolint:gosec,errcheck // G306: state file is config, not secrets; best-effort write
	_ = os.WriteFile(statePath, data, 0o644)
}
