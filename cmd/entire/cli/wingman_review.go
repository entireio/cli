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

const (
	// wingmanInitialDelay is how long to wait before starting the review,
	// letting the agent turn fully settle.
	wingmanInitialDelay = 10 * time.Second

	// wingmanApplyDelay is how long to wait after writing REVIEW.md
	// before attempting to auto-apply.
	wingmanApplyDelay = 30 * time.Second

	// wingmanReviewModel is the Claude model used for reviews.
	wingmanReviewModel = "sonnet"
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

func runWingmanReview(payloadJSON string) error {
	var payload WingmanPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	repoRoot := payload.RepoRoot
	if repoRoot == "" {
		return errors.New("repo_root is required in payload")
	}

	// Clean up lock file when review completes (regardless of success/failure)
	lockPath := filepath.Join(repoRoot, wingmanLockFile)
	defer os.Remove(lockPath)

	// Initial delay: let the agent turn fully settle
	time.Sleep(wingmanInitialDelay)

	// Compute diff using the base commit captured at trigger time
	diff, err := computeDiff(repoRoot, payload.BaseCommit)
	if err != nil {
		return fmt.Errorf("failed to compute diff: %w", err)
	}

	if strings.TrimSpace(diff) == "" {
		return nil // No changes to review
	}

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

	// Build review prompt
	prompt := buildReviewPrompt(payload.Prompts, fileList, diff)

	// Call Claude CLI for review
	reviewText, err := callClaudeForReview(prompt)
	if err != nil {
		return fmt.Errorf("failed to get review from Claude: %w", err)
	}

	// Write REVIEW.md
	reviewPath := filepath.Join(repoRoot, wingmanReviewFile)
	if err := os.MkdirAll(filepath.Dir(reviewPath), 0o750); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	//nolint:gosec // G306: review file is not secrets
	if err := os.WriteFile(reviewPath, []byte(reviewText), 0o644); err != nil {
		return fmt.Errorf("failed to write REVIEW.md: %w", err)
	}

	// Update dedup state
	allFilePaths := make([]string, 0, len(payload.ModifiedFiles)+len(payload.NewFiles)+len(payload.DeletedFiles))
	allFilePaths = append(allFilePaths, payload.ModifiedFiles...)
	allFilePaths = append(allFilePaths, payload.NewFiles...)
	allFilePaths = append(allFilePaths, payload.DeletedFiles...)

	// Save state from repo root context
	if err := os.Chdir(repoRoot); err == nil {
		_ = saveWingmanState(&WingmanState{ //nolint:errcheck // best-effort state save
			SessionID:     payload.SessionID,
			FilesHash:     computeFilesHash(allFilePaths),
			ReviewedAt:    time.Now(),
			ReviewApplied: false,
		})
	}

	// Auto-apply phase: wait then check if session is idle
	time.Sleep(wingmanApplyDelay)

	if !isSessionIdle(payload.SessionID) {
		return nil // User is active, leave REVIEW.md for notification
	}

	// Trigger auto-apply via claude --continue
	if err := triggerAutoApply(repoRoot); err != nil {
		return fmt.Errorf("failed to trigger auto-apply: %w", err)
	}

	return nil
}

// computeDiff gets the code changes to review. baseCommit is the HEAD hash
// captured at trigger time so the diff is stable even if HEAD moves during
// the initial delay (e.g., auto-commit creates another commit, or user commits).
func computeDiff(repoRoot string, baseCommit string) (string, error) {
	// Use the captured base commit when available for deterministic diffs
	diffRef := "HEAD"
	if baseCommit != "" {
		diffRef = baseCommit
	}

	// Try uncommitted changes against the base commit

	cmd := exec.CommandContext(context.Background(), "git", "diff", diffRef)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// If the ref doesn't exist (initial commit), try diff of staged/unstaged
		cmd2 := exec.CommandContext(context.Background(), "git", "diff")
		cmd2.Dir = repoRoot
		var stdout2 bytes.Buffer
		cmd2.Stdout = &stdout2
		if err2 := cmd2.Run(); err2 != nil {
			return "", fmt.Errorf("git diff failed: %w (stderr: %s)", err, stderr.String())
		}
		return stdout2.String(), nil
	}

	// If no uncommitted changes, the commit itself may contain the changes
	// (auto-commit strategy creates commits on the active branch)
	if strings.TrimSpace(stdout.String()) == "" {

		cmd2 := exec.CommandContext(context.Background(), "git", "diff", diffRef+"~1", diffRef)
		cmd2.Dir = repoRoot
		var stdout2 bytes.Buffer
		cmd2.Stdout = &stdout2
		if err := cmd2.Run(); err == nil && strings.TrimSpace(stdout2.String()) != "" {
			return stdout2.String(), nil
		}
	}

	return stdout.String(), nil
}

// callClaudeForReview calls the claude CLI to perform the review.
func callClaudeForReview(prompt string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "claude", "--print", "--output-format", "json", "--model", wingmanReviewModel, "--setting-sources", "")

	// Isolate from git repo to prevent hooks and index pollution
	cmd.Dir = os.TempDir()
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

// isSessionIdle checks if the given session is in the IDLE phase.
func isSessionIdle(sessionID string) bool {
	state, err := strategy.LoadSessionState(sessionID)
	if err != nil || state == nil {
		return false
	}
	return state.Phase == session.PhaseIdle
}

// triggerAutoApply spawns claude --continue to apply the review suggestions.
func triggerAutoApply(repoRoot string) error {
	applyPrompt := "Read .entire/REVIEW.md. Apply each suggestion that you agree with. When done, delete .entire/REVIEW.md."

	cmd := exec.CommandContext(context.Background(), "claude", "--continue", "-p", applyPrompt, "--setting-sources", "")
	cmd.Dir = repoRoot
	// Strip GIT_* env vars to prevent hook interference, but keep other env
	cmd.Env = wingmanStripGitEnv(os.Environ())

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
