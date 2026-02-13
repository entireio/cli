package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/spf13/cobra"
)

func newImportSessionCmd() *cobra.Command {
	var commitFlag string

	cmd := &cobra.Command{
		Use:   "import-session",
		Short: "Import Claude Code session transcript(s) into Entire checkpoints",
		Long: `Import Claude Code session transcript(s) into Entire checkpoints.

Use this to recover sessions that were not properly checkpointed (e.g., due to bugs)
or to import existing sessions when adopting Entire in an existing repository.

Each argument should be a path to a Claude Code JSONL transcript file (e.g., from
~/.claude/projects/<project>/sessions/*.jsonl).

By default, imports are associated with HEAD. Use --commit to target a specific commit.
When targeting a past commit, you will need to amend that commit to add the
Entire-Checkpoint trailer, which rewrites history and may require a force push.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImportSession(cmd, args, commitFlag)
		},
	}

	cmd.Flags().StringVar(&commitFlag, "commit", "", "Target commit (hash or ref) to associate the checkpoint with. Default is HEAD.")

	return cmd
}

func runImportSession(cmd *cobra.Command, sessionPaths []string, targetCommit string) error {
	ctx := context.Background()

	// Must be in a git repository
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		cmd.SilenceUsage = true
		return NewSilentError(fmt.Errorf("not a git repository: %w", err))
	}

	repo, err := strategy.OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Entire must be enabled
	s, err := settings.Load()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	if !s.Enabled {
		cmd.SilenceUsage = true
		return NewSilentError(errors.New("entire is not enabled. Run 'entire enable' first"))
	}

	// Resolve target commit
	hash, err := resolveCommit(repo, targetCommit)
	if err != nil {
		return fmt.Errorf("invalid --commit %q: %w", targetCommit, err)
	}

	// Get git author and branch for metadata
	authorName, authorEmail := strategy.GetGitAuthorFromRepo(repo)
	branchName := strategy.GetCurrentBranchName(repo)

	// Determine strategy name from settings
	strat := GetStrategy()
	strategyName := strat.Name()

	// Generate single checkpoint ID for this import (multi-session if multiple files)
	checkpointID, err := id.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate checkpoint ID: %w", err)
	}

	// Import each session file
	store := checkpoint.NewGitStore(repo)
	for i, sessionPath := range sessionPaths {
		if err := importOneSession(ctx, store, importSessionOpts{
			sessionPath:  sessionPath,
			checkpointID: checkpointID,
			sessionIndex: i,
			authorName:   authorName,
			authorEmail:  authorEmail,
			strategyName: strategyName,
			branchName:   branchName,
			repoRoot:     repoRoot,
		}); err != nil {
			return fmt.Errorf("import %q: %w", sessionPath, err)
		}
	}

	// Print success and instructions
	fmt.Fprintln(cmd.OutOrStdout(), "Imported", len(sessionPaths), "session(s) to checkpoint", checkpointID.String(), "on", hash.String()[:7])

	// Check if target commit has an Entire-Checkpoint trailer
	commitObj, err := repo.CommitObject(hash)
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}
	_, alreadyHasTrailer := trailers.ParseCheckpoint(commitObj.Message)

	if alreadyHasTrailer {
		fmt.Fprintf(cmd.OutOrStdout(), "Commit %s already has an Entire-Checkpoint trailer.\n", hash.String()[:7])
	} else {
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "To link this checkpoint to the commit, add the trailer:")
		if targetCommit == "" || targetCommit == "HEAD" {
			fmt.Fprintf(cmd.OutOrStdout(), "  git commit --amend -m \"$(git log -1 --format='%%B')\n%s: %s\"\n", trailers.CheckpointTrailerKey, checkpointID)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "  # Use interactive rebase to amend commit %s, then add:\n", hash.String()[:7])
			fmt.Fprintf(cmd.OutOrStdout(), "  # %s: %s\n", trailers.CheckpointTrailerKey, checkpointID)
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "Caution: Amending a past commit rewrites git history. You may need to force push to share with contributors.")
		}
	}

	return nil
}

type importSessionOpts struct {
	sessionPath  string
	checkpointID id.CheckpointID
	sessionIndex int
	authorName   string
	authorEmail  string
	strategyName string
	branchName   string
	repoRoot     string
}

func importOneSession(ctx context.Context, store *checkpoint.GitStore, opts importSessionOpts) error {
	data, err := os.ReadFile(opts.sessionPath)
	if err != nil {
		return fmt.Errorf("failed to read transcript: %w", err)
	}

	lines, err := claudecode.ParseTranscript(data)
	if err != nil {
		return fmt.Errorf("invalid Claude Code JSONL transcript: %w", err)
	}

	// Extract modified files and last user prompt
	modifiedFiles := claudecode.ExtractModifiedFiles(lines)
	lastPrompt := claudecode.ExtractLastUserPrompt(lines)

	// Normalize paths to repo-relative (required for checkpoint metadata)
	modifiedFiles = FilterAndNormalizePaths(modifiedFiles, opts.repoRoot)

	// Generate session ID - must be path-safe per validation
	sessionID := fmt.Sprintf("import-%s-%d", opts.checkpointID, opts.sessionIndex)

	// Build prompts slice (one entry for imported sessions)
	var prompts []string
	if lastPrompt != "" {
		prompts = []string{lastPrompt}
	}

	if err := store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID:     opts.checkpointID,
		SessionID:        sessionID,
		Strategy:         opts.strategyName,
		Branch:           opts.branchName,
		Transcript:       data,
		Prompts:          prompts,
		Context:          nil, // Import doesn't have context.md
		FilesTouched:     modifiedFiles,
		CheckpointsCount: 1,
		AuthorName:       opts.authorName,
		AuthorEmail:      opts.authorEmail,
		Agent:            agent.AgentTypeClaudeCode,
	}); err != nil {
		return fmt.Errorf("write committed: %w", err)
	}
	return nil
}

func resolveCommit(repo *git.Repository, ref string) (plumbing.Hash, error) {
	if ref == "" {
		ref = "HEAD"
	}
	h, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve revision: %w", err)
	}
	return *h, nil
}
