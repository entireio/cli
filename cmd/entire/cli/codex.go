package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codexcli"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newCodexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Codex CLI integration",
		Long:  "Run OpenAI Codex CLI commands with Entire checkpoint capture.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newCodexExecCmd())
	return cmd
}

func newCodexExecCmd() *cobra.Command {
	var (
		model   string
		sandbox string
	)

	cmd := &cobra.Command{
		Use:   "exec [flags] -- <prompt>",
		Short: "Run codex exec and capture a checkpoint",
		Long: `Run a non-interactive Codex session and capture the result as an Entire checkpoint.

This wraps 'codex exec --json' to capture the JSONL event stream, then
stores the session data using the configured Entire strategy.

The prompt can be passed after '--' or piped via stdin using '-'.

Examples:
  entire codex exec -- "fix the failing tests"
  entire codex exec --model o3 -- "refactor the auth module"
  echo "add error handling" | entire codex exec -- -`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCodexExec(cmd, args, model, sandbox)
		},
	}

	cmd.Flags().StringVarP(&model, "model", "m", "", "Override Codex model")
	cmd.Flags().StringVarP(&sandbox, "sandbox", "s", "", "Sandbox policy (read-only, workspace-write, danger-full-access)")

	return cmd
}

func runCodexExec(cmd *cobra.Command, args []string, model, sandbox string) error {
	// Verify Entire is enabled
	enabled, err := IsEnabled()
	if err == nil && !enabled {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Entire is not enabled. Run 'entire enable' first.")
		return NewSilentError(errors.New("entire not enabled"))
	}

	// Verify codex binary exists
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Codex CLI not found in PATH. Install it from https://github.com/openai/codex")
		return NewSilentError(errors.New("codex not found"))
	}

	// Verify we're in a git repository
	if _, err := paths.RepoRoot(); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
		return NewSilentError(errors.New("not a git repository"))
	}

	prompt := strings.Join(args, " ")

	// Capture pre-execution state
	preState, captureErr := capturePreExecState()
	if captureErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to capture pre-exec state: %v\n", captureErr)
	}

	// Build codex command args
	codexArgs := []string{"exec", "--json"}
	if model != "" {
		codexArgs = append(codexArgs, "--model", model)
	}
	if sandbox != "" {
		codexArgs = append(codexArgs, "--sandbox", sandbox)
	}
	codexArgs = append(codexArgs, prompt)

	// Create temp file for capturing JSONL output
	tmpFile, err := os.CreateTemp("", "entire-codex-*.jsonl")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// Run codex exec, tee stdout to temp file and stderr to user
	//nolint:gosec // codexPath is resolved from LookPath
	codexCmd := exec.CommandContext(cmd.Context(), codexPath, codexArgs...)
	codexCmd.Stdin = cmd.InOrStdin()
	codexCmd.Stderr = cmd.ErrOrStderr()
	codexCmd.Stdout = tmpFile

	fmt.Fprintf(cmd.ErrOrStderr(), "Running: codex %s\n", strings.Join(codexArgs, " "))

	codexErr := codexCmd.Run()
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Read captured JSONL
	data, readErr := os.ReadFile(tmpPath) //nolint:gosec // tmpPath is from CreateTemp
	if readErr != nil {
		return fmt.Errorf("failed to read captured output: %w", readErr)
	}

	if len(data) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "No output captured from Codex.")
		if codexErr != nil {
			return codexErr //nolint:wrapcheck // propagate codex exit code
		}
		return nil
	}

	// Parse the event stream
	session, parseErr := codexcli.ParseEventStream(data)
	if parseErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to parse Codex output: %v\n", parseErr)
		if codexErr != nil {
			return codexErr //nolint:wrapcheck // propagate codex exit code
		}
		return nil
	}

	// Create checkpoint from parsed session
	if err := createCodexCheckpoint(cmd, session, data, prompt, preState); err != nil {
		return fmt.Errorf("failed to create checkpoint: %w", err)
	}

	// Propagate Codex exit code
	if codexErr != nil {
		return codexErr //nolint:wrapcheck // propagate codex exit code
	}

	return nil
}

// preExecState stores the state captured before running Codex.
type preExecState struct {
	untrackedFiles []string
}

func capturePreExecState() (*preExecState, error) {
	files, err := getUntrackedFilesForState()
	if err != nil {
		return nil, err
	}
	return &preExecState{untrackedFiles: files}, nil
}

func createCodexCheckpoint(
	cmd *cobra.Command,
	session *codexcli.ParsedSession,
	rawData []byte,
	prompt string,
	preState *preExecState,
) error {
	stderr := cmd.ErrOrStderr()

	// Determine session ID from Codex thread ID
	sessionID := session.ThreadID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Create session metadata directory
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Write raw transcript
	transcriptFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := os.WriteFile(transcriptFile, rawData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	fmt.Fprintf(stderr, "Saved transcript to: %s/%s\n", sessionDir, paths.TranscriptFileName)

	// Write prompts file
	promptFile := filepath.Join(sessionDirAbs, paths.PromptFileName)
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}

	// Write summary
	summaryFile := filepath.Join(sessionDirAbs, paths.SummaryFileName)
	summary := codexcli.ExtractLastMessage(session)
	if err := os.WriteFile(summaryFile, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}

	// Get modified files from the event stream
	modifiedFiles := session.ModifiedFiles

	// Compute new and deleted files from git status
	var previouslyUntracked []string
	if preState != nil {
		previouslyUntracked = preState.untrackedFiles
	}
	changes, err := DetectFileChanges(previouslyUntracked)
	if err != nil {
		fmt.Fprintf(stderr, "Warning: failed to detect file changes: %v\n", err)
	}

	// Get repo root for path normalization
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repo root: %w", err)
	}

	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
	}

	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		fmt.Fprintln(stderr, "No files were modified during this session.")
		return nil
	}

	fmt.Fprintf(stderr, "Files modified (%d):\n", len(relModifiedFiles))
	for _, file := range relModifiedFiles {
		fmt.Fprintf(stderr, "  - %s\n", file)
	}
	if len(relNewFiles) > 0 {
		fmt.Fprintf(stderr, "New files (%d):\n", len(relNewFiles))
		for _, file := range relNewFiles {
			fmt.Fprintf(stderr, "  + %s\n", file)
		}
	}
	if len(relDeletedFiles) > 0 {
		fmt.Fprintf(stderr, "Deleted files (%d):\n", len(relDeletedFiles))
		for _, file := range relDeletedFiles {
			fmt.Fprintf(stderr, "  - %s\n", file)
		}
	}

	// Generate commit message
	commitMessage := generateCodexCommitMessage(prompt)
	fmt.Fprintf(stderr, "Commit message: %s\n", commitMessage)

	// Get git author
	author, err := GetGitAuthor()
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get strategy and save
	strat := GetStrategy()
	if err := strat.EnsureSetup(); err != nil {
		fmt.Fprintf(stderr, "Warning: failed to ensure strategy setup: %v\n", err)
	}

	ctx := strategy.SaveContext{
		SessionID:      sessionID,
		ModifiedFiles:  relModifiedFiles,
		NewFiles:       relNewFiles,
		DeletedFiles:   relDeletedFiles,
		MetadataDir:    sessionDir,
		MetadataDirAbs: sessionDirAbs,
		CommitMessage:  commitMessage,
		TranscriptPath: transcriptFile,
		AuthorName:     author.Name,
		AuthorEmail:    author.Email,
		AgentType:      agent.AgentTypeCodex,
		TokenUsage:     session.TokenUsage,
	}

	if err := strat.SaveChanges(ctx); err != nil {
		return fmt.Errorf("failed to save changes: %w", err)
	}

	fmt.Fprintln(stderr, "Checkpoint saved.")

	// Print Codex's summary response so the user sees what it did
	if summary != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", summary)
	}

	return nil
}

func generateCodexCommitMessage(prompt string) string {
	if prompt != "" {
		cleaned := cleanPromptForCommit(prompt)
		if cleaned != "" {
			return cleaned
		}
	}
	return "Codex CLI session updates"
}
