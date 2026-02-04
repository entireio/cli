package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/gitutil"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/spf13/cobra"
)

func newDebugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "debug",
		Short:  "Debug commands for troubleshooting",
		Hidden: true, // Hidden from help output
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newDebugAutoCommitCmd())

	return cmd
}

func newDebugAutoCommitCmd() *cobra.Command {
	var transcriptPath string

	cmd := &cobra.Command{
		Use:   "auto-commit",
		Short: "Show whether current state would trigger an auto-commit",
		Long: `Analyzes the current session state and configuration to determine
if the Stop hook would create an auto-commit.

This simulates what the Stop hook checks:
- Current session and pre-prompt state
- Modified files from transcript (if --transcript provided)
- New files (current untracked - pre-prompt untracked)
- Deleted files (tracked files that were removed)

Without --transcript, shows git status changes instead.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDebugAutoCommit(cmd.OutOrStdout(), transcriptPath)
		},
	}

	cmd.Flags().StringVarP(&transcriptPath, "transcript", "t", "", "Path to transcript file (.jsonl) to parse for modified files")

	return cmd
}

func runDebugAutoCommit(w io.Writer, transcriptPath string) error {
	// Check if we're in a git repository
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		fmt.Fprintln(w, "Not in a git repository")
		return nil //nolint:nilerr // not being in a git repo is expected, not an error for status check
	}
	fmt.Fprintf(w, "Repository: %s\n\n", repoRoot)

	// Print strategy info
	strat := GetStrategy()
	isAutoCommit := strat.Name() == strategy.StrategyNameAutoCommit
	printStrategyInfo(w, strat, isAutoCommit)

	// Print session state
	currentSession := printSessionState(w)

	// Auto-detect transcript if not provided
	if transcriptPath == "" && currentSession != "" {
		detected, detectErr := findTranscriptForSession(currentSession, repoRoot)
		if detectErr != nil {
			fmt.Fprintf(w, "\nCould not auto-detect transcript: %v\n", detectErr)
		} else if detected != "" {
			transcriptPath = detected
			fmt.Fprintf(w, "\nAuto-detected transcript: %s\n", transcriptPath)
		}
	}

	// Print file changes and get total
	fmt.Fprintln(w, "\n=== File Changes ===")
	var totalChanges int
	if transcriptPath != "" {
		totalChanges = printTranscriptChanges(w, transcriptPath, currentSession, repoRoot)
	} else {
		var err error
		totalChanges, err = printGitStatusChanges(w)
		if err != nil {
			return err
		}
	}

	// Print decision
	printDecision(w, isAutoCommit, strat.Name(), totalChanges)

	// Print transcript location help if we couldn't find one
	if transcriptPath == "" {
		printTranscriptHelp(w)
	}

	return nil
}

func printStrategyInfo(w io.Writer, strat strategy.Strategy, isAutoCommit bool) {
	fmt.Fprintf(w, "Strategy: %s\n", strat.Name())
	fmt.Fprintf(w, "Auto-commit strategy: %v\n", isAutoCommit)

	_, branchName, err := gitutil.IsOnDefaultBranch()
	if err != nil {
		fmt.Fprintf(w, "Branch: (unable to determine: %v)\n\n", err)
	} else {
		fmt.Fprintf(w, "Branch: %s\n\n", branchName)
	}
}

func printSessionState(w io.Writer) string {
	fmt.Fprintln(w, "=== Session State ===")

	currentSession, err := paths.ReadCurrentSession()
	switch {
	case err != nil:
		fmt.Fprintf(w, "Current session: (error: %v)\n", err)
		return ""
	case currentSession == "":
		fmt.Fprintln(w, "Current session: (none - no active session)")
		return ""
	default:
		fmt.Fprintf(w, "Current session: %s\n", currentSession)
		printPrePromptState(w, currentSession)
		return currentSession
	}
}

func printPrePromptState(w io.Writer, sessionID string) {
	preState, err := LoadPrePromptState(sessionID)
	switch {
	case err != nil:
		fmt.Fprintf(w, "Pre-prompt state: (error: %v)\n", err)
	case preState == nil:
		fmt.Fprintln(w, "Pre-prompt state: (none captured)")
	default:
		fmt.Fprintf(w, "Pre-prompt state: captured at %s\n", preState.Timestamp)
		fmt.Fprintf(w, "  Pre-existing untracked files: %d\n", len(preState.UntrackedFiles))
		printUntrackedFilesSummary(w, preState.UntrackedFiles)
	}
}

func printUntrackedFilesSummary(w io.Writer, files []string) {
	if len(files) == 0 {
		return
	}
	if len(files) <= 10 {
		for _, f := range files {
			fmt.Fprintf(w, "    - %s\n", f)
		}
	} else {
		for _, f := range files[:5] {
			fmt.Fprintf(w, "    - %s\n", f)
		}
		fmt.Fprintf(w, "    ... and %d more\n", len(files)-5)
	}
}

func printTranscriptChanges(w io.Writer, transcriptPath, currentSession, repoRoot string) int {
	fmt.Fprintf(w, "\nParsing transcript: %s\n", transcriptPath)

	var modifiedFromTranscript, newFiles, deletedFiles []string

	// Parse transcript
	transcript, err := parseTranscript(transcriptPath)
	if err != nil {
		fmt.Fprintf(w, "  Error parsing transcript: %v\n", err)
	} else {
		modifiedFromTranscript = extractModifiedFiles(transcript)
		fmt.Fprintf(w, "  Found %d modified files in transcript\n", len(modifiedFromTranscript))
	}

	// Compute new and deleted files (single git status call)
	// Load preState only if we have an active session (needed for new file detection)
	var preState *PrePromptState
	if currentSession != "" {
		var loadErr error
		preState, loadErr = LoadPrePromptState(currentSession)
		if loadErr != nil {
			fmt.Fprintf(w, "  Error loading pre-prompt state: %v\n", loadErr)
		}
	}
	// Always call ComputeFileChanges - deleted files don't depend on preState
	newFiles, deletedFiles, err = ComputeFileChanges(preState)
	if err != nil {
		fmt.Fprintf(w, "  Error computing file changes: %v\n", err)
	}

	// Filter and normalize paths
	modifiedFromTranscript = FilterAndNormalizePaths(modifiedFromTranscript, repoRoot)
	newFiles = FilterAndNormalizePaths(newFiles, repoRoot)
	deletedFiles = FilterAndNormalizePaths(deletedFiles, repoRoot)

	// Print files
	printFileList(w, "Modified (from transcript)", "M", modifiedFromTranscript)
	printFileList(w, "New files (created during session)", "+", newFiles)
	printFileList(w, "Deleted files", "D", deletedFiles)

	totalChanges := len(modifiedFromTranscript) + len(newFiles) + len(deletedFiles)
	if totalChanges == 0 {
		fmt.Fprintln(w, "\nNo changes detected from transcript")
	}

	return totalChanges
}

func printGitStatusChanges(w io.Writer) (int, error) {
	fmt.Fprintln(w, "\n(No --transcript provided, showing git status instead)")
	fmt.Fprintln(w, "Note: Stop hook uses transcript parsing, not git status")

	modifiedFiles, untrackedFiles, deletedFiles, stagedFiles, err := getFileChanges()
	if err != nil {
		return 0, fmt.Errorf("failed to get file changes: %w", err)
	}

	printFileList(w, "Staged files", "+", stagedFiles)
	printFileList(w, "Modified files", "M", modifiedFiles)
	printFileList(w, "Untracked files", "?", untrackedFiles)
	printFileList(w, "Deleted files", "D", deletedFiles)

	totalChanges := len(modifiedFiles) + len(untrackedFiles) + len(deletedFiles) + len(stagedFiles)
	if totalChanges == 0 {
		fmt.Fprintln(w, "\nNo changes detected in git status")
	}

	return totalChanges, nil
}

func printFileList(w io.Writer, label, prefix string, files []string) {
	if len(files) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d):\n", label, len(files))
	for _, f := range files {
		fmt.Fprintf(w, "  %s %s\n", prefix, f)
	}
}

func printDecision(w io.Writer, isAutoCommit bool, stratName string, totalChanges int) {
	fmt.Fprintln(w, "\n=== Auto-Commit Decision ===")

	wouldCommit := isAutoCommit && totalChanges > 0

	if wouldCommit {
		fmt.Fprintln(w, "Result: YES - Auto-commit would be triggered")
		fmt.Fprintf(w, "  %d file(s) would be committed\n", totalChanges)
		return
	}

	fmt.Fprintln(w, "Result: NO - Auto-commit would NOT be triggered")
	fmt.Fprintln(w, "Reasons:")
	if !isAutoCommit {
		fmt.Fprintf(w, "  - Strategy is not auto-commit (using %s)\n", stratName)
	}
	if totalChanges == 0 {
		fmt.Fprintln(w, "  - No file changes to commit")
	}
}

func printTranscriptHelp(w io.Writer) {
	fmt.Fprintln(w, "\n=== Finding Transcript ===")
	fmt.Fprintln(w, "Claude Code transcripts are typically at:")
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(w, "  ~/.claude/projects/*/sessions/*.jsonl")
	} else {
		fmt.Fprintf(w, "  %s/.claude/projects/*/sessions/*.jsonl\n", homeDir)
	}
}

// getFileChanges returns the current file changes from git status.
// Returns (modifiedFiles, untrackedFiles, deletedFiles, stagedFiles, error)
func getFileChanges() ([]string, []string, []string, []string, error) {
	repo, err := gitutil.OpenRepository()
	if err != nil {
		return nil, nil, nil, nil, err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("getting worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("getting status: %w", err)
	}

	var modifiedFiles, untrackedFiles, deletedFiles, stagedFiles []string

	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Check staging area first
		switch st.Staging {
		case git.Added, git.Modified:
			stagedFiles = append(stagedFiles, file)
			continue
		case git.Deleted:
			deletedFiles = append(deletedFiles, file)
			continue
		case git.Unmodified, git.Renamed, git.Copied, git.UpdatedButUnmerged, git.Untracked:
			// Fall through to check worktree status
		}

		// Check worktree status
		switch st.Worktree {
		case git.Modified:
			modifiedFiles = append(modifiedFiles, file)
		case git.Untracked:
			untrackedFiles = append(untrackedFiles, file)
		case git.Deleted:
			deletedFiles = append(deletedFiles, file)
		case git.Unmodified, git.Added, git.Renamed, git.Copied, git.UpdatedButUnmerged:
			// No action needed
		}
	}

	// Sort for consistent output
	sort.Strings(modifiedFiles)
	sort.Strings(untrackedFiles)
	sort.Strings(deletedFiles)
	sort.Strings(stagedFiles)

	return modifiedFiles, untrackedFiles, deletedFiles, stagedFiles, nil
}

// findTranscriptForSession attempts to find the transcript file for a session.
// Returns the path if found, empty string if not found, or error on failure.
func findTranscriptForSession(sessionID, repoRoot string) (string, error) {
	// Try to get agent type from session state
	sessionState, err := strategy.LoadSessionState(sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to load session state: %w", err)
	}

	var ag agent.Agent
	if sessionState != nil && sessionState.AgentType != "" {
		ag, err = agent.GetByAgentType(sessionState.AgentType)
		if err != nil {
			return "", fmt.Errorf("failed to get agent for type %q: %w", sessionState.AgentType, err)
		}
	} else {
		return "", fmt.Errorf("failed to get agent from sessionID: %s", sessionID)
	}

	// Get the session directory for this agent
	sessionDir, err := ag.GetSessionDir(repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to get session dir: %w", err)
	}

	// Extract the agent-specific session ID (removes date prefix)
	agentSessionID := ag.ExtractAgentSessionID(sessionID)

	// Build the transcript path
	transcriptPath := filepath.Join(sessionDir, agentSessionID+".jsonl")

	// Check if it exists
	if _, err := os.Stat(transcriptPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil // Not found, but not an error
		}
		return "", fmt.Errorf("failed to stat transcript: %w", err)
	}

	return transcriptPath, nil
}
