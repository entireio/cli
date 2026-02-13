package cli

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

// WingmanPayload is the data passed to the detached review subprocess.
type WingmanPayload struct {
	SessionID     string   `json:"session_id"`
	RepoRoot      string   `json:"repo_root"`
	BaseCommit    string   `json:"base_commit"`
	ModifiedFiles []string `json:"modified_files"`
	NewFiles      []string `json:"new_files"`
	DeletedFiles  []string `json:"deleted_files"`
	Prompts       []string `json:"prompts"`
	CommitMessage string   `json:"commit_message"`
}

// WingmanState tracks deduplication and review state.
type WingmanState struct {
	SessionID        string     `json:"session_id"`
	FilesHash        string     `json:"files_hash"`
	ReviewedAt       time.Time  `json:"reviewed_at"`
	ReviewApplied    bool       `json:"review_applied"`
	ApplyAttemptedAt *time.Time `json:"apply_attempted_at,omitempty"`
}

//go:embed wingman_instruction.md
var wingmanApplyInstruction string

const (
	wingmanStateFile  = ".entire/wingman-state.json"
	wingmanReviewFile = ".entire/REVIEW.md"
	wingmanLockFile   = ".entire/wingman.lock"

	// wingmanStaleReviewTTL is the maximum age of a pending REVIEW.md before
	// it's considered stale and automatically cleaned up.
	wingmanStaleReviewTTL = 1 * time.Hour
)

func newWingmanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wingman",
		Short: "Automated code review for agent sessions",
		Long: `Wingman provides automated code review after agent turns.

When enabled, wingman automatically reviews code changes made by agents,
writes suggestions to .entire/REVIEW.md, and optionally triggers the agent
to apply them.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newWingmanEnableCmd())
	cmd.AddCommand(newWingmanDisableCmd())
	cmd.AddCommand(newWingmanStatusCmd())
	cmd.AddCommand(newWingmanReviewCmd())
	cmd.AddCommand(newWingmanApplyCmd())

	return cmd
}

func newWingmanEnableCmd() *cobra.Command {
	var useLocal bool

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable wingman auto-review",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := paths.RepoRoot(); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			// Load merged settings to check preconditions
			merged, err := settings.Load()
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}

			if !merged.Enabled {
				fmt.Fprintln(cmd.ErrOrStderr(), "Entire is not enabled. Run 'entire enable' first.")
				return NewSilentError(errors.New("entire not enabled"))
			}

			// Load the target file specifically so we don't bloat it with merged values
			s, err := loadSettingsTarget(useLocal)
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}

			if s.StrategyOptions == nil {
				s.StrategyOptions = make(map[string]any)
			}
			s.StrategyOptions["wingman"] = map[string]any{"enabled": true}

			if err := saveSettingsTarget(s, useLocal); err != nil {
				return fmt.Errorf("failed to save settings: %w", err)
			}

			msg := "Wingman enabled. Code changes will be automatically reviewed after agent turns."
			if useLocal {
				msg += " (saved to settings.local.json)"
			}
			fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}

	cmd.Flags().BoolVar(&useLocal, "local", false, "Write to settings.local.json instead of settings.json")

	return cmd
}

func newWingmanDisableCmd() *cobra.Command {
	var useLocal bool

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable wingman auto-review",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadSettingsTarget(useLocal)
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}

			if s.StrategyOptions == nil {
				s.StrategyOptions = make(map[string]any)
			}
			s.StrategyOptions["wingman"] = map[string]any{"enabled": false}

			if err := saveSettingsTarget(s, useLocal); err != nil {
				return fmt.Errorf("failed to save settings: %w", err)
			}

			// Clean up pending review file if it exists
			reviewPath, err := paths.AbsPath(wingmanReviewFile)
			if err == nil {
				_ = os.Remove(reviewPath)
			}

			msg := "Wingman disabled."
			if useLocal {
				msg += " (saved to settings.local.json)"
			}
			fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}

	cmd.Flags().BoolVar(&useLocal, "local", false, "Write to settings.local.json instead of settings.json")

	return cmd
}

// loadSettingsTarget loads settings from the appropriate file based on the --local flag.
// When local is true, loads from settings.local.json only (without merging).
// When local is false, loads the merged settings (project + local).
func loadSettingsTarget(local bool) (*settings.EntireSettings, error) {
	if !local {
		s, err := settings.Load()
		if err != nil {
			return nil, fmt.Errorf("loading settings: %w", err)
		}
		return s, nil
	}
	absPath, err := paths.AbsPath(settings.EntireSettingsLocalFile)
	if err != nil {
		absPath = settings.EntireSettingsLocalFile
	}
	s, err := settings.LoadFromFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("loading local settings: %w", err)
	}
	return s, nil
}

// saveSettingsTarget saves settings to the appropriate file based on the --local flag.
func saveSettingsTarget(s *settings.EntireSettings, local bool) error {
	if local {
		if err := settings.SaveLocal(s); err != nil {
			return fmt.Errorf("saving local settings: %w", err)
		}
		return nil
	}
	if err := settings.Save(s); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	return nil
}

func newWingmanStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show wingman status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings.Load()
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}

			if s.IsWingmanEnabled() {
				fmt.Fprintln(cmd.OutOrStdout(), "Wingman: enabled")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Wingman: disabled")
			}

			// Show last review info if available
			state, err := loadWingmanState()
			if err == nil && state != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Last review: %s\n", state.ReviewedAt.Format(time.RFC3339))
				if state.ReviewApplied {
					fmt.Fprintln(cmd.OutOrStdout(), "Status: applied")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "Status: pending")
				}
			}

			// Check for pending REVIEW.md
			reviewPath, err := paths.AbsPath(wingmanReviewFile)
			if err == nil {
				if _, statErr := os.Stat(reviewPath); statErr == nil {
					fmt.Fprintln(cmd.OutOrStdout(), "Pending review: .entire/REVIEW.md")
				}
			}

			return nil
		},
	}
}

// triggerWingmanReview checks preconditions and spawns the detached review process.
func triggerWingmanReview(payload WingmanPayload) {
	// Prevent infinite recursion: if we're inside a wingman auto-apply,
	// don't trigger another review. The env var is set by triggerAutoApply.
	if os.Getenv("ENTIRE_WINGMAN_APPLY") != "" {
		return
	}

	logCtx := logging.WithComponent(context.Background(), "wingman")
	repoRoot := payload.RepoRoot

	totalFiles := len(payload.ModifiedFiles) + len(payload.NewFiles) + len(payload.DeletedFiles)
	logging.Info(logCtx, "wingman trigger evaluating",
		slog.String("session_id", payload.SessionID),
		slog.Int("file_count", totalFiles),
	)

	// Check if a pending REVIEW.md already exists for the current session
	if shouldSkipPendingReview(repoRoot, payload.SessionID) {
		logging.Info(logCtx, "wingman skipped: pending review exists for current session")
		fmt.Fprintf(os.Stderr, "[wingman] Pending review exists, skipping\n")
		return
	}

	// Atomic lock file prevents concurrent review spawns. O_CREATE|O_EXCL
	// is atomic on all platforms, avoiding the TOCTOU race of Stat+WriteFile.
	lockPath := filepath.Join(repoRoot, wingmanLockFile)
	if !acquireWingmanLock(lockPath, payload.SessionID) {
		logging.Info(logCtx, "wingman skipped: review already in progress")
		fmt.Fprintf(os.Stderr, "[wingman] Review in progress, skipping\n")
		return
	}

	// Dedup check: compute hash of sorted file paths
	allFiles := make([]string, 0, len(payload.ModifiedFiles)+len(payload.NewFiles)+len(payload.DeletedFiles))
	allFiles = append(allFiles, payload.ModifiedFiles...)
	allFiles = append(allFiles, payload.NewFiles...)
	allFiles = append(allFiles, payload.DeletedFiles...)
	filesHash := computeFilesHash(allFiles)

	state, _ := loadWingmanState() //nolint:errcheck // best-effort dedup
	if state != nil && state.FilesHash == filesHash && state.SessionID == payload.SessionID {
		logging.Info(logCtx, "wingman skipped: dedup hash match",
			slog.String("files_hash", filesHash[:12]),
		)
		fmt.Fprintf(os.Stderr, "[wingman] Already reviewed these changes, skipping\n")
		return
	}

	// Capture HEAD at trigger time so the detached review diffs against
	// the correct commit even if HEAD moves during the initial delay.
	payload.BaseCommit = resolveHEAD(repoRoot)
	logging.Debug(logCtx, "wingman captured base commit",
		slog.String("base_commit", payload.BaseCommit),
	)

	// Write payload to a temp file instead of passing as a CLI argument,
	// which can exceed OS argv limits (~128KB Linux, ~256KB macOS) with
	// many files or long prompts.
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		logging.Error(logCtx, "wingman failed to marshal payload", slog.Any("error", err))
		fmt.Fprintf(os.Stderr, "[wingman] Failed to marshal payload: %v\n", err)
		_ = os.Remove(lockPath)
		return
	}
	payloadPath := filepath.Join(repoRoot, ".entire", "wingman-payload.json")
	//nolint:gosec // G306: payload file is not secrets
	if err := os.WriteFile(payloadPath, payloadJSON, 0o644); err != nil {
		logging.Error(logCtx, "wingman failed to write payload file", slog.Any("error", err))
		fmt.Fprintf(os.Stderr, "[wingman] Failed to write payload file: %v\n", err)
		_ = os.Remove(lockPath)
		return
	}

	// Spawn detached review process with path to payload file
	spawnDetachedWingmanReview(repoRoot, payloadPath)
	logging.Info(logCtx, "wingman review spawned",
		slog.String("session_id", payload.SessionID),
		slog.String("base_commit", payload.BaseCommit),
		slog.Int("file_count", totalFiles),
	)
	fmt.Fprintf(os.Stderr, "[wingman] Review starting in background...\n")
}

// triggerWingmanFromCommit builds a wingman payload from the HEAD commit and
// triggers a review. Used by the git post-commit hook for manual-commit strategy
// where files are committed by the user (not by SaveChanges).
func triggerWingmanFromCommit() {
	// Prevent infinite recursion: skip if inside wingman auto-apply
	if os.Getenv("ENTIRE_WINGMAN_APPLY") != "" {
		return
	}
	if !settings.IsWingmanEnabled() {
		return
	}

	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return
	}

	head := resolveHEAD(repoRoot)
	if head == "" {
		return
	}

	// Get changed files from the commit
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	//nolint:gosec // G204: head is from git rev-parse, not user input
	cmd := exec.CommandContext(ctx, "git", "diff-tree", "--no-commit-id", "--name-status", "-r", head)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return
	}

	var modified, newFiles, deleted []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if len(line) < 3 {
			continue
		}
		status := line[0]
		file := strings.TrimSpace(line[1:])
		switch status {
		case 'M':
			modified = append(modified, file)
		case 'A':
			newFiles = append(newFiles, file)
		case 'D':
			deleted = append(deleted, file)
		}
	}

	if len(modified)+len(newFiles)+len(deleted) == 0 {
		return
	}

	// Get commit message
	//nolint:gosec // G204: head is from git rev-parse, not user input
	msgCmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%B", head)
	msgCmd.Dir = repoRoot
	msgOut, _ := msgCmd.Output() //nolint:errcheck // best-effort commit message
	commitMessage := strings.TrimSpace(string(msgOut))

	sessionID := strategy.FindMostRecentSession()

	triggerWingmanReview(WingmanPayload{
		SessionID:     sessionID,
		RepoRoot:      repoRoot,
		ModifiedFiles: modified,
		NewFiles:      newFiles,
		DeletedFiles:  deleted,
		CommitMessage: commitMessage,
	})
}

// staleLockThreshold is how old a lock file can be before we consider it stale
// (e.g., the detached process was SIGKILLed and the defer never ran).
const staleLockThreshold = 30 * time.Minute

// acquireWingmanLock atomically creates the lock file. Returns true if acquired.
// If the lock already exists but is older than staleLockThreshold, it is removed
// and re-acquired (handles crashed detached processes).
func acquireWingmanLock(lockPath, sessionID string) bool {
	//nolint:gosec // G304: lockPath is constructed from repoRoot + constant
	f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			fmt.Fprintf(os.Stderr, "[wingman] Failed to create lock: %v\n", err)
			return false
		}
		// Lock exists — check if it's stale
		info, statErr := os.Stat(lockPath)
		if statErr != nil || time.Since(info.ModTime()) <= staleLockThreshold {
			fmt.Fprintf(os.Stderr, "[wingman] Review already in progress, skipping\n")
			return false
		}
		fmt.Fprintf(os.Stderr, "[wingman] Removing stale lock (age: %s)\n",
			time.Since(info.ModTime()).Round(time.Second))
		_ = os.Remove(lockPath)
		// Retry the atomic create
		//nolint:gosec // G304: lockPath is constructed from repoRoot + constant
		f, err = os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[wingman] Failed to create lock after stale removal: %v\n", err)
			return false
		}
	}
	_, _ = f.WriteString(sessionID) //nolint:errcheck // best-effort session ID write
	_ = f.Close()
	return true
}

// resolveHEAD returns the current HEAD commit hash, or empty string on error.
func resolveHEAD(repoRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// computeFilesHash returns a SHA256 hex digest of the sorted file paths.
// Uses null byte separator (impossible in filenames) to avoid ambiguity.
func computeFilesHash(files []string) string {
	sorted := make([]string, len(files))
	copy(sorted, files)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(h[:])
}

// loadWingmanState loads the wingman state from .entire/wingman-state.json.
func loadWingmanState() (*WingmanState, error) {
	statePath, err := paths.AbsPath(wingmanStateFile)
	if err != nil {
		return nil, fmt.Errorf("resolving wingman state path: %w", err)
	}

	data, err := os.ReadFile(statePath) //nolint:gosec // path is repo-relative
	if err != nil {
		return nil, fmt.Errorf("reading wingman state: %w", err)
	}

	var state WingmanState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing wingman state: %w", err)
	}
	return &state, nil
}

// saveWingmanState saves the wingman state to .entire/wingman-state.json.
func saveWingmanState(state *WingmanState) error {
	statePath, err := paths.AbsPath(wingmanStateFile)
	if err != nil {
		return fmt.Errorf("resolving wingman state path: %w", err)
	}

	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating wingman state directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling wingman state: %w", err)
	}

	//nolint:gosec // G306: state file is config, not secrets
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		return fmt.Errorf("writing wingman state: %w", err)
	}
	return nil
}

// loadWingmanStateDirect reads the wingman state file directly from a known
// path under repoRoot. Returns nil if the file doesn't exist or can't be parsed.
func loadWingmanStateDirect(repoRoot string) *WingmanState {
	statePath := filepath.Join(repoRoot, wingmanStateFile)
	data, err := os.ReadFile(statePath) //nolint:gosec // path is repo-relative constant
	if err != nil {
		return nil
	}
	var state WingmanState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	return &state
}

// newWingmanApplyCmd returns a hidden subcommand used by the stop hook to
// apply a pending REVIEW.md via claude --continue in a detached subprocess.
func newWingmanApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__apply",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runWingmanApply(args[0])
		},
	}
}

// shouldSkipPendingReview checks whether a pending REVIEW.md should prevent
// a new review from being triggered. It cleans up stale/orphaned reviews.
//
// Returns true only when REVIEW.md exists AND belongs to the current session
// AND is younger than wingmanStaleReviewTTL.
func shouldSkipPendingReview(repoRoot, currentSessionID string) bool {
	reviewPath := filepath.Join(repoRoot, wingmanReviewFile)
	if _, err := os.Stat(reviewPath); err != nil {
		return false // No REVIEW.md, don't skip
	}

	// REVIEW.md exists — check state to determine if it's current or stale
	state := loadWingmanStateDirect(repoRoot)
	if state == nil {
		// Orphan: REVIEW.md without state file — clean up
		_ = os.Remove(reviewPath)
		return false
	}

	if state.SessionID != currentSessionID {
		// Different session — stale review, clean up
		_ = os.Remove(reviewPath)
		return false
	}

	if time.Since(state.ReviewedAt) > wingmanStaleReviewTTL {
		// Same session but too old — clean up
		_ = os.Remove(reviewPath)
		return false
	}

	return true // Current session, fresh review — skip
}
