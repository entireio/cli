package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
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
	SessionID     string    `json:"session_id"`
	FilesHash     string    `json:"files_hash"`
	ReviewedAt    time.Time `json:"reviewed_at"`
	ReviewApplied bool      `json:"review_applied"`
}

const (
	wingmanStateFile  = ".entire/wingman-state.json"
	wingmanReviewFile = ".entire/REVIEW.md"
	wingmanLockFile   = ".entire/wingman.lock"
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

	return cmd
}

func newWingmanEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Enable wingman auto-review",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := paths.RepoRoot(); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			s, err := settings.Load()
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}

			if !s.Enabled {
				fmt.Fprintln(cmd.ErrOrStderr(), "Entire is not enabled. Run 'entire enable' first.")
				return NewSilentError(errors.New("entire not enabled"))
			}

			if s.StrategyOptions == nil {
				s.StrategyOptions = make(map[string]any)
			}
			s.StrategyOptions["wingman"] = map[string]any{"enabled": true}

			if err := settings.Save(s); err != nil {
				return fmt.Errorf("failed to save settings: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Wingman enabled. Code changes will be automatically reviewed after agent turns.")
			return nil
		},
	}
}

func newWingmanDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Disable wingman auto-review",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings.Load()
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}

			if s.StrategyOptions == nil {
				s.StrategyOptions = make(map[string]any)
			}
			s.StrategyOptions["wingman"] = map[string]any{"enabled": false}

			if err := settings.Save(s); err != nil {
				return fmt.Errorf("failed to save settings: %w", err)
			}

			// Clean up pending review file if it exists
			reviewPath, err := paths.AbsPath(wingmanReviewFile)
			if err == nil {
				_ = os.Remove(reviewPath)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Wingman disabled.")
			return nil
		},
	}
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
	repoRoot := payload.RepoRoot

	// Check if a pending REVIEW.md already exists
	reviewPath := filepath.Join(repoRoot, wingmanReviewFile)
	if _, err := os.Stat(reviewPath); err == nil {
		fmt.Fprintf(os.Stderr, "[wingman] Pending review exists, skipping\n")
		return
	}

	// Lock file prevents concurrent review spawns. Multiple rapid stop hooks
	// could race past the dedup check (which reads stale state) before any
	// review completes and updates the state file.
	lockPath := filepath.Join(repoRoot, wingmanLockFile)
	if _, err := os.Stat(lockPath); err == nil {
		fmt.Fprintf(os.Stderr, "[wingman] Review already in progress, skipping\n")
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
		fmt.Fprintf(os.Stderr, "[wingman] Already reviewed these changes, skipping\n")
		return
	}

	// Capture HEAD at trigger time so the detached review diffs against
	// the correct commit even if HEAD moves during the initial delay.
	payload.BaseCommit = resolveHEAD(repoRoot)

	// Write lock file synchronously before spawning to prevent races
	//nolint:gosec // G306: lock file is not secrets
	_ = os.WriteFile(lockPath, []byte(payload.SessionID), 0o644) //nolint:errcheck // best-effort lock

	// Marshal payload
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[wingman] Failed to marshal payload: %v\n", err)
		_ = os.Remove(lockPath)
		return
	}

	// Spawn detached review process
	spawnDetachedWingmanReview(string(payloadJSON))
	fmt.Fprintf(os.Stderr, "[wingman] Review starting in background...\n")
}

// resolveHEAD returns the current HEAD commit hash, or empty string on error.
func resolveHEAD(repoRoot string) string {
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// computeFilesHash returns a SHA256 hex digest of the sorted file paths.
func computeFilesHash(files []string) string {
	sorted := make([]string, len(files))
	copy(sorted, files)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
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
