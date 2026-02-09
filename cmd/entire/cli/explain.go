package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/transcript"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// interaction holds a single prompt and its responses for display.
type interaction struct {
	Prompt    string
	Responses []string // Multiple responses can occur between tool calls
	Files     []string
}

// associatedCommit holds information about a git commit associated with a checkpoint.
type associatedCommit struct {
	SHA      string
	ShortSHA string
	Message  string
	Author   string
	Date     time.Time
}

// checkpointDetail holds detailed information about a checkpoint for display.
type checkpointDetail struct {
	Index            int
	ShortID          string
	Timestamp        time.Time
	IsTaskCheckpoint bool
	Message          string
	// Interactions contains all prompt/response pairs in this checkpoint.
	// Most strategies have one, but shadow condensations may have multiple.
	Interactions []interaction
	// Files is the aggregate list of all files modified (for backwards compat)
	Files []string
}

func newExplainCmd() *cobra.Command {
	var sessionFlag string
	var commitFlag string
	var checkpointFlag string
	var noPagerFlag bool
	var shortFlag bool
	var fullFlag bool
	var rawTranscriptFlag bool
	var generateFlag bool
	var forceFlag bool
	var searchAllFlag bool

	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Explain a session, commit, or checkpoint",
		Long: `Explain provides human-readable context about sessions, commits, and checkpoints.

Use this command to understand what happened during agent-driven development,
either for self-review or to understand a teammate's work.

By default, shows checkpoints on the current branch. Use flags to filter or
explain specific items.

Filtering the list view:
  --session      Filter checkpoints by session ID (or prefix)

Viewing specific items:
  --commit       Explain a specific commit (shows its associated checkpoint)
  --checkpoint   Explain a specific checkpoint by ID

Output verbosity levels (for --checkpoint):
  Default:         Detailed view with scoped prompts (ID, session, tokens, intent, prompts, files)
  --short:         Summary only (ID, session, timestamp, tokens, intent)
  --full:          Parsed full transcript (all prompts/responses from entire session)
  --raw-transcript: Raw transcript file (JSONL format)

Summary generation (for --checkpoint):
  --generate    Generate an AI summary for the checkpoint
  --force       Regenerate even if a summary already exists (requires --generate)

Performance options:
  --search-all  Remove branch/depth limits when searching for commits (may be slow)

Checkpoint detail view shows:
  - Author who created the checkpoint
  - Associated git commits that reference the checkpoint
  - Prompts and responses from the session

Note: --session filters the list view; --commit and --checkpoint are mutually exclusive.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument %q\nHint: use --checkpoint, --session, or --commit to specify what to explain", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if Entire is disabled
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			// Validate flag dependencies
			if generateFlag && checkpointFlag == "" {
				return errors.New("--generate requires --checkpoint/-c flag")
			}
			if forceFlag && !generateFlag {
				return errors.New("--force requires --generate flag")
			}
			if rawTranscriptFlag && checkpointFlag == "" {
				return errors.New("--raw-transcript requires --checkpoint/-c flag")
			}

			// Convert short flag to verbose (verbose = !short)
			verbose := !shortFlag
			return runExplain(cmd.OutOrStdout(), cmd.ErrOrStderr(), sessionFlag, commitFlag, checkpointFlag, noPagerFlag, verbose, fullFlag, rawTranscriptFlag, generateFlag, forceFlag, searchAllFlag)
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Filter checkpoints by session ID (or prefix)")
	cmd.Flags().StringVar(&commitFlag, "commit", "", "Explain a specific commit (SHA or ref)")
	cmd.Flags().StringVarP(&checkpointFlag, "checkpoint", "c", "", "Explain a specific checkpoint (ID or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	cmd.Flags().BoolVarP(&shortFlag, "short", "s", false, "Show summary only (omit prompts and files)")
	cmd.Flags().BoolVar(&fullFlag, "full", false, "Show parsed full transcript (all prompts/responses)")
	cmd.Flags().BoolVar(&rawTranscriptFlag, "raw-transcript", false, "Show raw transcript file (JSONL format)")
	cmd.Flags().BoolVar(&generateFlag, "generate", false, "Generate an AI summary for the checkpoint")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Regenerate summary even if one already exists (requires --generate)")
	cmd.Flags().BoolVar(&searchAllFlag, "search-all", false, "Search all commits (no branch/depth limit, may be slow)")

	// Make --short, --full, and --raw-transcript mutually exclusive
	cmd.MarkFlagsMutuallyExclusive("short", "full", "raw-transcript")
	// --generate and --raw-transcript are incompatible (summary would be generated but not shown)
	cmd.MarkFlagsMutuallyExclusive("generate", "raw-transcript")

	return cmd
}

// runExplain routes to the appropriate explain function based on flags.
func runExplain(w, errW io.Writer, sessionID, commitRef, checkpointID string, noPager, verbose, full, rawTranscript, generate, force, searchAll bool) error {
	// Count mutually exclusive flags (--commit and --checkpoint are mutually exclusive)
	// --session is now a filter for the list view, not a separate mode
	flagCount := 0
	if commitRef != "" {
		flagCount++
	}
	if checkpointID != "" {
		flagCount++
	}
	// If --session is combined with --commit or --checkpoint, that's still an error
	if sessionID != "" && flagCount > 0 {
		return errors.New("cannot specify multiple of --session, --commit, --checkpoint")
	}
	if flagCount > 1 {
		return errors.New("cannot specify multiple of --session, --commit, --checkpoint")
	}

	// Route to appropriate handler
	if commitRef != "" {
		return runExplainCommit(w, commitRef, noPager, verbose, full, searchAll)
	}
	if checkpointID != "" {
		return runExplainCheckpoint(w, errW, checkpointID, noPager, verbose, full, rawTranscript, generate, force, searchAll)
	}

	// Default or with session filter: show list view (optionally filtered by session)
	return runExplainBranchWithFilter(w, noPager, sessionID)
}

// runExplainCheckpoint explains a specific checkpoint.
// Supports both committed checkpoints (by checkpoint ID) and temporary checkpoints (by git SHA).
// First tries to match committed checkpoints, then falls back to temporary checkpoints.
// When generate is true, generates an AI summary for the checkpoint.
// When force is true, regenerates even if a summary already exists.
// When rawTranscript is true, outputs only the raw transcript file (JSONL format).
// When searchAll is true, searches all commits without branch/depth limits (used for finding associated commits).
func runExplainCheckpoint(w, errW io.Writer, checkpointIDPrefix string, noPager, verbose, full, rawTranscript, generate, force, searchAll bool) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	store := checkpoint.NewGitStore(repo)

	// First, try to find in committed checkpoints by checkpoint ID prefix
	committed, err := store.ListCommitted(context.Background())
	if err != nil {
		return fmt.Errorf("failed to list checkpoints: %w", err)
	}

	// Collect all matching checkpoint IDs to detect ambiguity
	var matches []id.CheckpointID
	for _, info := range committed {
		if strings.HasPrefix(info.CheckpointID.String(), checkpointIDPrefix) {
			matches = append(matches, info.CheckpointID)
		}
	}

	var fullCheckpointID id.CheckpointID
	switch len(matches) {
	case 0:
		// Not found in committed, try temporary checkpoints by git SHA
		if generate {
			return fmt.Errorf("cannot generate summary for temporary checkpoint %s (only committed checkpoints supported)", checkpointIDPrefix)
		}
		output, found := explainTemporaryCheckpoint(w, repo, store, checkpointIDPrefix, verbose, full, rawTranscript)
		if found {
			outputExplainContent(w, output, noPager)
			return nil
		}
		// If output is non-empty, it contains an error message (e.g., ambiguous prefix)
		if output != "" {
			return errors.New(output)
		}
		return fmt.Errorf("checkpoint not found: %s", checkpointIDPrefix)
	case 1:
		fullCheckpointID = matches[0]
	default:
		// Ambiguous prefix - show up to 5 examples
		examples := make([]string, 0, 5)
		for i := 0; i < len(matches) && i < 5; i++ {
			examples = append(examples, matches[i].String())
		}
		return fmt.Errorf("ambiguous checkpoint prefix %q matches %d checkpoints: %s", checkpointIDPrefix, len(matches), strings.Join(examples, ", "))
	}

	// Load checkpoint summary
	summary, err := store.ReadCommitted(context.Background(), fullCheckpointID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if summary == nil {
		return fmt.Errorf("checkpoint not found: %s", fullCheckpointID)
	}

	// Load latest session content (needed for transcript and metadata)
	content, err := store.ReadLatestSessionContent(context.Background(), fullCheckpointID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint content: %w", err)
	}

	// Handle summary generation
	if generate {
		if err := generateCheckpointSummary(w, errW, store, fullCheckpointID, summary, content, force); err != nil {
			return err
		}
		// Reload the content to get the updated summary
		content, err = store.ReadLatestSessionContent(context.Background(), fullCheckpointID)
		if err != nil {
			return fmt.Errorf("failed to reload checkpoint: %w", err)
		}
	}

	// Handle raw transcript output
	if rawTranscript {
		if len(content.Transcript) == 0 {
			return fmt.Errorf("checkpoint %s has no transcript", fullCheckpointID)
		}
		// Output raw transcript directly (no pager, no formatting)
		if _, err = w.Write(content.Transcript); err != nil {
			return fmt.Errorf("failed to write transcript: %w", err)
		}
		return nil
	}

	// Look up the author for this checkpoint (best-effort, ignore errors)
	author, _ := store.GetCheckpointAuthor(context.Background(), fullCheckpointID) //nolint:errcheck // Author is optional

	// Find associated commits (git commits with matching Entire-Checkpoint trailer)
	associatedCommits, _ := getAssociatedCommits(repo, fullCheckpointID, searchAll) //nolint:errcheck // Best-effort

	// Format and output
	output := formatCheckpointOutput(summary, content, fullCheckpointID, associatedCommits, author, verbose, full)
	outputExplainContent(w, output, noPager)
	return nil
}

// generateCheckpointSummary generates an AI summary for a checkpoint and persists it.
// The summary is generated from the scoped transcript (only this checkpoint's portion),
// not the entire session transcript.
func generateCheckpointSummary(w, _ io.Writer, store *checkpoint.GitStore, checkpointID id.CheckpointID, cpSummary *checkpoint.CheckpointSummary, content *checkpoint.SessionContent, force bool) error {
	// Check if summary already exists
	if content.Metadata.Summary != nil && !force {
		return fmt.Errorf("checkpoint %s already has a summary (use --force to regenerate)", checkpointID)
	}

	// Check if transcript exists
	if len(content.Transcript) == 0 {
		return fmt.Errorf("checkpoint %s has no transcript to summarize", checkpointID)
	}

	// Scope the transcript to only this checkpoint's portion
	scopedTranscript := scopeTranscriptForCheckpoint(content.Transcript, content.Metadata.GetTranscriptStart())
	if len(scopedTranscript) == 0 {
		return fmt.Errorf("checkpoint %s has no transcript content for this checkpoint (scoped)", checkpointID)
	}

	// Generate summary using shared helper
	ctx := context.Background()
	logging.Info(ctx, "generating checkpoint summary")

	summary, err := summarize.GenerateFromTranscript(ctx, scopedTranscript, cpSummary.FilesTouched, nil)
	if err != nil {
		return fmt.Errorf("failed to generate summary: %w", err)
	}

	// Persist the summary
	if err := store.UpdateSummary(ctx, checkpointID, summary); err != nil {
		return fmt.Errorf("failed to save summary: %w", err)
	}

	fmt.Fprintln(w, "✓ Summary generated and saved")
	return nil
}

// explainTemporaryCheckpoint finds and formats a temporary checkpoint by shadow commit hash prefix.
// Returns the formatted output and whether the checkpoint was found.
// Searches ALL shadow branches, not just the one for current HEAD, to find checkpoints
// created from different base commits (e.g., if HEAD advanced since session start).
// The writer w is used for raw transcript output to bypass the pager.
func explainTemporaryCheckpoint(w io.Writer, repo *git.Repository, store *checkpoint.GitStore, shaPrefix string, verbose, full, rawTranscript bool) (string, bool) {
	// List temporary checkpoints from ALL shadow branches
	// This ensures we find checkpoints even if HEAD has advanced since the session started
	tempCheckpoints, err := store.ListAllTemporaryCheckpoints(context.Background(), "", branchCheckpointsLimit)
	if err != nil {
		return "", false
	}

	// Find checkpoints matching the SHA prefix - check for ambiguity
	var matches []checkpoint.TemporaryCheckpointInfo
	for _, tc := range tempCheckpoints {
		if strings.HasPrefix(tc.CommitHash.String(), shaPrefix) {
			matches = append(matches, tc)
		}
	}

	if len(matches) == 0 {
		return "", false
	}

	if len(matches) > 1 {
		// Multiple matches - return ambiguous error (consistent with committed checkpoint behavior)
		var sb strings.Builder
		fmt.Fprintf(&sb, "ambiguous checkpoint prefix %q matches %d temporary checkpoints:\n", shaPrefix, len(matches))
		for _, m := range matches {
			shortID := m.CommitHash.String()[:7]
			fmt.Fprintf(&sb, "  %s  %s  session %s\n",
				shortID,
				m.Timestamp.Format("2006-01-02 15:04:05"),
				m.SessionID)
		}
		// Return as "not found" with error message - caller will use this as error
		return sb.String(), false
	}

	tc := matches[0]

	// Handle raw transcript output
	if rawTranscript {
		transcriptBytes, transcriptErr := store.GetTranscriptFromCommit(tc.CommitHash, tc.MetadataDir, agent.AgentTypeUnknown)
		if transcriptErr != nil || len(transcriptBytes) == 0 {
			// Return specific error message (consistent with committed checkpoints)
			return fmt.Sprintf("checkpoint %s has no transcript", tc.CommitHash.String()[:7]), false
		}
		// Write directly to writer (no pager, no formatting) - matches committed checkpoint behavior
		if _, writeErr := fmt.Fprint(w, string(transcriptBytes)); writeErr != nil {
			return fmt.Sprintf("failed to write transcript: %v", writeErr), false
		}
		return "", true
	}

	// Found exactly one match - read metadata from shadow branch commit tree
	shadowCommit, commitErr := repo.CommitObject(tc.CommitHash)
	if commitErr != nil {
		return "", false
	}

	shadowTree, treeErr := shadowCommit.Tree()
	if treeErr != nil {
		return "", false
	}

	// Read prompts from shadow branch
	sessionPrompt := strategy.ReadSessionPromptFromTree(shadowTree, tc.MetadataDir)

	// Build output similar to formatCheckpointOutput but for temporary
	var sb strings.Builder
	shortID := tc.CommitHash.String()[:7]
	fmt.Fprintf(&sb, "Checkpoint: %s [temporary]\n", shortID)
	fmt.Fprintf(&sb, "Session: %s\n", tc.SessionID)
	fmt.Fprintf(&sb, "Created: %s\n", tc.Timestamp.Format("2006-01-02 15:04:05"))
	sb.WriteString("\n")

	// Intent from prompt
	intent := "(not available)"
	if sessionPrompt != "" {
		lines := strings.Split(sessionPrompt, "\n")
		if len(lines) > 0 && lines[0] != "" {
			intent = strategy.TruncateDescription(lines[0], maxIntentDisplayLength)
		}
	}
	fmt.Fprintf(&sb, "Intent: %s\n", intent)
	sb.WriteString("Outcome: (not generated)\n")

	// Transcript section: full shows entire session, verbose shows checkpoint scope
	// For temporary checkpoints, load transcript and compute scope from parent commit
	var fullTranscript []byte
	var scopedTranscript []byte
	if full || verbose {
		fullTranscript, _ = store.GetTranscriptFromCommit(tc.CommitHash, tc.MetadataDir, agent.AgentTypeUnknown) //nolint:errcheck // Best-effort

		if verbose && len(fullTranscript) > 0 {
			// Compute scoped transcript by finding where parent's transcript ended
			// Each shadow branch commit has the full transcript up to that point,
			// so we diff against parent to get just this checkpoint's activity
			scopedTranscript = fullTranscript // Default to full if no parent
			if shadowCommit.NumParents() > 0 {
				if parent, parentErr := shadowCommit.Parent(0); parentErr == nil {
					parentTranscript, _ := store.GetTranscriptFromCommit(parent.Hash, tc.MetadataDir, agent.AgentTypeUnknown) //nolint:errcheck // Best-effort
					if len(parentTranscript) > 0 {
						// Count lines in parent transcript to know where to slice from
						parentLineCount := countLines(parentTranscript)
						scopedTranscript = transcript.SliceFromLine(fullTranscript, parentLineCount)
					}
				}
			}
		}
	}
	appendTranscriptSection(&sb, verbose, full, fullTranscript, scopedTranscript, sessionPrompt)

	return sb.String(), true
}

// getAssociatedCommits finds git commits that reference the given checkpoint ID.
// Searches commits on the current branch for Entire-Checkpoint trailer matches.
// When searchAll is true, removes the commit scan limit (may be slow).
func getAssociatedCommits(repo *git.Repository, checkpointID id.CheckpointID, searchAll bool) ([]associatedCommit, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Determine scan limit
	limit := commitScanLimit
	if searchAll {
		limit = 0 // No limit
	}

	// Check if on default branch for filtering
	isOnDefault, _ := strategy.IsOnDefaultBranch(repo)
	var mainBranchHash plumbing.Hash
	if !isOnDefault {
		mainBranchHash = strategy.GetMainBranchHash(repo)
	}

	// Precompute main branch commits for filtering (same logic as getBranchCheckpoints)
	reachableFromMain := make(map[plumbing.Hash]bool)
	if mainBranchHash != plumbing.ZeroHash {
		mainIter, mainErr := repo.Log(&git.LogOptions{From: mainBranchHash})
		if mainErr == nil {
			mainCount := 0
			_ = mainIter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort main branch detection
				mainCount++
				if mainCount > 1000 {
					return errStopIteration
				}
				reachableFromMain[c.Hash] = true
				return nil
			})
			mainIter.Close()
		}
	}

	iter, err := repo.Log(&git.LogOptions{
		From:  head.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get commit log: %w", err)
	}
	defer iter.Close()

	commits := []associatedCommit{} // Initialize as empty slice, not nil (nil means "not searched")
	count := 0
	targetID := checkpointID.String()

	err = iter.ForEach(func(c *object.Commit) error {
		if limit > 0 && count >= limit {
			return errStopIteration
		}
		count++

		// Skip commits on main when on feature branch (same filtering as list view)
		if len(reachableFromMain) > 0 && reachableFromMain[c.Hash] {
			return nil
		}

		// Check for matching checkpoint trailer
		cpID, found := trailers.ParseCheckpoint(c.Message)
		if !found || cpID.String() != targetID {
			return nil
		}

		fullSHA := c.Hash.String()
		shortSHA := fullSHA
		if len(fullSHA) >= 7 {
			shortSHA = fullSHA[:7]
		}

		commits = append(commits, associatedCommit{
			SHA:      fullSHA,
			ShortSHA: shortSHA,
			Message:  strings.Split(c.Message, "\n")[0],
			Author:   c.Author.Name,
			Date:     c.Author.When,
		})

		return nil
	})

	if err != nil && !errors.Is(err, errStopIteration) {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	return commits, nil
}

// scopeTranscriptForCheckpoint slices a transcript to include only the lines
// relevant to a specific checkpoint, starting from linesAtStart.
// This allows showing only what happened during a checkpoint, not the entire session.
func scopeTranscriptForCheckpoint(fullTranscript []byte, linesAtStart int) []byte {
	return transcript.SliceFromLine(fullTranscript, linesAtStart)
}

// extractPromptsFromTranscript extracts user prompts from transcript bytes.
// Returns a slice of prompt strings.
func extractPromptsFromTranscript(transcriptBytes []byte) []string {
	if len(transcriptBytes) == 0 {
		return nil
	}

	condensed, err := summarize.BuildCondensedTranscriptFromBytes(transcriptBytes)
	if err != nil {
		return nil
	}

	var prompts []string
	for _, entry := range condensed {
		if entry.Type == summarize.EntryTypeUser && entry.Content != "" {
			prompts = append(prompts, entry.Content)
		}
	}
	return prompts
}

// formatCheckpointOutput formats checkpoint data based on verbosity level.
// When verbose is false: summary only (ID, session, timestamp, tokens, intent).
// When verbose is true: adds files, associated commits, and scoped transcript for this checkpoint.
// When full is true: shows parsed full session transcript instead of scoped transcript.
//
// Transcript scope is controlled by CheckpointTranscriptStart in metadata, which indicates
// where this checkpoint's content begins in the full session transcript.
//
// Author is displayed when available (only for committed checkpoints).
// Associated commits are git commits that reference this checkpoint via Entire-Checkpoint trailer.
func formatCheckpointOutput(summary *checkpoint.CheckpointSummary, content *checkpoint.SessionContent, checkpointID id.CheckpointID, associatedCommits []associatedCommit, author checkpoint.Author, verbose, full bool) string {
	var sb strings.Builder
	meta := content.Metadata

	// Scope the transcript to this checkpoint's portion
	// If CheckpointTranscriptStart > 0, we slice the transcript to only include
	// lines from that point onwards (excluding earlier checkpoint content)
	scopedTranscript := scopeTranscriptForCheckpoint(content.Transcript, meta.GetTranscriptStart())

	// Extract prompts from the scoped transcript for intent extraction
	scopedPrompts := extractPromptsFromTranscript(scopedTranscript)

	// Header - always shown
	// Note: CheckpointID is always exactly 12 characters, matching checkpointIDDisplayLength
	fmt.Fprintf(&sb, "Checkpoint: %s\n", checkpointID)
	fmt.Fprintf(&sb, "Session: %s\n", meta.SessionID)
	fmt.Fprintf(&sb, "Created: %s\n", meta.CreatedAt.Format("2006-01-02 15:04:05"))

	// Author (only for committed checkpoints with known author)
	if author.Name != "" {
		fmt.Fprintf(&sb, "Author: %s <%s>\n", author.Name, author.Email)
	}

	// Token usage - prefer content metadata, fall back to summary
	tokenUsage := meta.TokenUsage
	if tokenUsage == nil && summary != nil {
		tokenUsage = summary.TokenUsage
	}
	if tokenUsage != nil {
		totalTokens := tokenUsage.InputTokens + tokenUsage.CacheCreationTokens +
			tokenUsage.CacheReadTokens + tokenUsage.OutputTokens
		fmt.Fprintf(&sb, "Tokens: %d\n", totalTokens)
	}

	// Associated commits section
	if len(associatedCommits) > 0 {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "Commits: (%d)\n", len(associatedCommits))
		for _, c := range associatedCommits {
			fmt.Fprintf(&sb, "  %s %s %s\n", c.ShortSHA, c.Date.Format("2006-01-02"), c.Message)
		}
	} else if associatedCommits != nil {
		// associatedCommits is non-nil but empty - show "no commits found" message
		sb.WriteString("\nCommits: No commits found on this branch\n")
	}

	sb.WriteString("\n")

	// Intent and Outcome from AI summary, or fallback to prompt text
	if meta.Summary != nil {
		fmt.Fprintf(&sb, "Intent: %s\n", meta.Summary.Intent)
		fmt.Fprintf(&sb, "Outcome: %s\n", meta.Summary.Outcome)
	} else {
		// Fallback: use first line of scoped prompts for intent,
		// or fall back to result.Prompts for backwards compatibility with older checkpoints
		intent := "(not generated)"
		if len(scopedPrompts) > 0 && scopedPrompts[0] != "" {
			intent = strategy.TruncateDescription(scopedPrompts[0], maxIntentDisplayLength)
		} else if content.Prompts != "" {
			// Backwards compatibility: use stored prompts if no transcript available
			lines := strings.Split(content.Prompts, "\n")
			if len(lines) > 0 && lines[0] != "" {
				intent = strategy.TruncateDescription(lines[0], maxIntentDisplayLength)
			}
		}
		fmt.Fprintf(&sb, "Intent: %s\n", intent)
		sb.WriteString("Outcome: (not generated)\n")
	}

	// Verbose: add learnings, friction, files, and scoped transcript
	if verbose || full {
		// AI Summary details (learnings, friction, open items)
		if meta.Summary != nil {
			formatSummaryDetails(&sb, meta.Summary)
		}

		sb.WriteString("\n")

		// Files section
		if len(meta.FilesTouched) > 0 {
			fmt.Fprintf(&sb, "Files: (%d)\n", len(meta.FilesTouched))
			for _, file := range meta.FilesTouched {
				fmt.Fprintf(&sb, "  - %s\n", file)
			}
		} else {
			sb.WriteString("Files: (none)\n")
		}
	}

	// Transcript section: full shows entire session, verbose shows checkpoint scope
	appendTranscriptSection(&sb, verbose, full, content.Transcript, scopedTranscript, content.Prompts)

	return sb.String()
}

// appendTranscriptSection appends the appropriate transcript section to the builder
// based on verbosity level. Full mode shows the entire session, verbose shows checkpoint scope.
// fullTranscript is the entire session transcript, scopedContent is either scoped transcript bytes
// or a pre-formatted string (for backwards compat), and scopedFallback is used when scoped parsing fails.
func appendTranscriptSection(sb *strings.Builder, verbose, full bool, fullTranscript, scopedTranscript []byte, scopedFallback string) {
	switch {
	case full:
		sb.WriteString("\n")
		sb.WriteString("Transcript (full session):\n")
		sb.WriteString(formatTranscriptBytes(fullTranscript, ""))

	case verbose:
		sb.WriteString("\n")
		sb.WriteString("Transcript (checkpoint scope):\n")
		sb.WriteString(formatTranscriptBytes(scopedTranscript, scopedFallback))
	}
}

// formatTranscriptBytes formats transcript bytes into a human-readable string.
// It parses the JSONL transcript and formats it using the condensed format.
// The fallback is used for backwards compatibility when transcript parsing fails or is empty.
func formatTranscriptBytes(transcriptBytes []byte, fallback string) string {
	if len(transcriptBytes) == 0 {
		if fallback != "" {
			return fallback + "\n"
		}
		return "  (none)\n"
	}

	condensed, err := summarize.BuildCondensedTranscriptFromBytes(transcriptBytes)
	if err != nil || len(condensed) == 0 {
		if fallback != "" {
			return fallback + "\n"
		}
		return "  (failed to parse transcript)\n"
	}

	input := summarize.Input{Transcript: condensed}
	return summarize.FormatCondensedTranscript(input)
}

// formatSummaryDetails formats the detailed sections of an AI summary.
func formatSummaryDetails(sb *strings.Builder, summary *checkpoint.Summary) {
	// Learnings section
	hasLearnings := len(summary.Learnings.Repo) > 0 ||
		len(summary.Learnings.Code) > 0 ||
		len(summary.Learnings.Workflow) > 0

	if hasLearnings {
		sb.WriteString("\nLearnings:\n")

		if len(summary.Learnings.Repo) > 0 {
			sb.WriteString("  Repository:\n")
			for _, learning := range summary.Learnings.Repo {
				fmt.Fprintf(sb, "    - %s\n", learning)
			}
		}

		if len(summary.Learnings.Code) > 0 {
			sb.WriteString("  Code:\n")
			for _, learning := range summary.Learnings.Code {
				if learning.Line > 0 {
					if learning.EndLine > 0 {
						fmt.Fprintf(sb, "    - %s:%d-%d: %s\n", learning.Path, learning.Line, learning.EndLine, learning.Finding)
					} else {
						fmt.Fprintf(sb, "    - %s:%d: %s\n", learning.Path, learning.Line, learning.Finding)
					}
				} else {
					fmt.Fprintf(sb, "    - %s: %s\n", learning.Path, learning.Finding)
				}
			}
		}

		if len(summary.Learnings.Workflow) > 0 {
			sb.WriteString("  Workflow:\n")
			for _, learning := range summary.Learnings.Workflow {
				fmt.Fprintf(sb, "    - %s\n", learning)
			}
		}
	}

	// Friction section
	if len(summary.Friction) > 0 {
		sb.WriteString("\nFriction:\n")
		for _, item := range summary.Friction {
			fmt.Fprintf(sb, "  - %s\n", item)
		}
	}

	// Open items section
	if len(summary.OpenItems) > 0 {
		sb.WriteString("\nOpen Items:\n")
		for _, item := range summary.OpenItems {
			fmt.Fprintf(sb, "  - %s\n", item)
		}
	}
}

// runExplainDefault shows all checkpoints on the current branch.
// This is the default view when no flags are provided.
func runExplainDefault(w io.Writer, noPager bool) error {
	return runExplainBranchDefault(w, noPager)
}

// branchCheckpointsLimit is the max checkpoints to show in branch view
const branchCheckpointsLimit = 100

// commitScanLimit is how far back to scan git history for checkpoints
const commitScanLimit = 500

// consecutiveMainLimit stops scanning after this many consecutive commits on main
// (indicates we've likely exhausted feature branch commits)
const consecutiveMainLimit = 100

// errStopIteration is used to stop commit iteration early
var errStopIteration = errors.New("stop iteration")

// getBranchCheckpoints returns checkpoints relevant to the current branch.
// This is strategy-agnostic - it queries checkpoints directly from the checkpoint store.
//
// Behavior:
//   - On feature branches: only show checkpoints unique to this branch (not in main)
//   - On default branch (main/master): show all checkpoints in history (up to limit)
//   - Includes both committed checkpoints (entire/checkpoints/v1) and temporary checkpoints (shadow branches)
func getBranchCheckpoints(repo *git.Repository, limit int) ([]strategy.RewindPoint, error) {
	store := checkpoint.NewGitStore(repo)

	// Get all committed checkpoints for lookup
	committedInfos, err := store.ListCommitted(context.Background())
	if err != nil {
		committedInfos = nil // Continue without committed checkpoints
	}

	// Build map of checkpoint ID -> committed info
	committedByID := make(map[id.CheckpointID]checkpoint.CommittedInfo)
	for _, info := range committedInfos {
		if !info.CheckpointID.IsEmpty() {
			committedByID[info.CheckpointID] = info
		}
	}

	head, err := repo.Head()
	if err != nil {
		// Unborn HEAD (no commits yet) - return empty list instead of erroring
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return []strategy.RewindPoint{}, nil
		}
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Check if we're on the default branch (use repo-aware version)
	isOnDefault, _ := strategy.IsOnDefaultBranch(repo)
	var mainBranchHash plumbing.Hash
	if !isOnDefault {
		// Prefer the actual default branch name (which may not be main/master)
		if defaultBranchName := strategy.GetDefaultBranchName(repo); defaultBranchName != "" {
			// Try local default branch first: refs/heads/<name>
			ref, refErr := repo.Reference(plumbing.ReferenceName("refs/heads/"+defaultBranchName), true)
			if refErr != nil {
				// Fall back to remote default branch: refs/remotes/origin/<name>
				ref, refErr = repo.Reference(plumbing.ReferenceName("refs/remotes/origin/"+defaultBranchName), true)
			}
			if refErr == nil {
				mainBranchHash = ref.Hash()
			}
		}

		// Fall back to main/master detection if default branch resolution failed
		if mainBranchHash == plumbing.ZeroHash {
			mainBranchHash = strategy.GetMainBranchHash(repo)
		}
	}

	// Precompute commits reachable from main (for O(1) filtering instead of O(N) per commit)
	// This changes the filtering from O(N*M) to O(N+M) where N=commits scanned, M=main history depth
	reachableFromMain := make(map[plumbing.Hash]bool)
	if mainBranchHash != plumbing.ZeroHash {
		mainIter, mainErr := repo.Log(&git.LogOptions{From: mainBranchHash})
		if mainErr == nil {
			mainCount := 0
			_ = mainIter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort
				mainCount++
				if mainCount > 1000 { // Same depth limit as before
					return errStopIteration
				}
				reachableFromMain[c.Hash] = true
				return nil
			})
			mainIter.Close()
		}
	}

	// Walk git history and collect checkpoints
	iter, err := repo.Log(&git.LogOptions{
		From:  head.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get commit log: %w", err)
	}
	defer iter.Close()

	var points []strategy.RewindPoint
	count := 0
	consecutiveMainCount := 0

	err = iter.ForEach(func(c *object.Commit) error {
		if count >= commitScanLimit {
			return errStopIteration
		}
		count++

		// On feature branches, skip commits that are reachable from main
		// (but continue scanning - there may be more feature branch commits)
		if len(reachableFromMain) > 0 {
			if reachableFromMain[c.Hash] {
				consecutiveMainCount++
				if consecutiveMainCount >= consecutiveMainLimit {
					return errStopIteration // Likely exhausted feature branch commits
				}
				return nil // Skip this commit, continue scanning
			}
			consecutiveMainCount = 0 // Reset on feature branch commit
		}

		// Extract checkpoint ID from Entire-Checkpoint trailer
		cpID, found := trailers.ParseCheckpoint(c.Message)
		if !found {
			return nil // No checkpoint trailer, continue
		}

		// Look up checkpoint info
		cpInfo, found := committedByID[cpID]
		if !found {
			return nil // Checkpoint not in store, continue
		}

		// Create rewind point from committed info
		message := strings.Split(c.Message, "\n")[0]
		point := strategy.RewindPoint{
			ID:               c.Hash.String(),
			Message:          message,
			Date:             c.Committer.When,
			IsLogsOnly:       true, // Committed checkpoints are logs-only
			CheckpointID:     cpID,
			SessionID:        cpInfo.SessionID,
			IsTaskCheckpoint: cpInfo.IsTask,
			ToolUseID:        cpInfo.ToolUseID,
			Agent:            cpInfo.Agent,
		}
		// Read session prompt from metadata branch (best-effort)
		content, _ := store.ReadLatestSessionContent(context.Background(), cpID) //nolint:errcheck  // Best-effort
		if content != nil {
			// Scope the transcript to this checkpoint's portion
			// If CheckpointTranscriptStart > 0, we slice the transcript to only include
			// lines from that point onwards (excluding earlier checkpoint content)
			scopedTranscript := scopeTranscriptForCheckpoint(content.Transcript, content.Metadata.GetTranscriptStart())
			// Extract prompts from the scoped transcript (not the full session's prompts)
			scopedPrompts := extractPromptsFromTranscript(scopedTranscript)
			if len(scopedPrompts) > 0 && scopedPrompts[0] != "" {
				point.SessionPrompt = scopedPrompts[0]
			}
		}

		points = append(points, point)
		return nil
	})

	if err != nil && !errors.Is(err, errStopIteration) {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	// Get temporary checkpoints from ALL shadow branches whose base commit is reachable from HEAD.
	tempPoints := getReachableTemporaryCheckpoints(repo, store, head.Hash(), isOnDefault, limit)
	points = append(points, tempPoints...)

	// Sort by date, most recent first
	sort.Slice(points, func(i, j int) bool {
		return points[i].Date.After(points[j].Date)
	})

	// Apply limit
	if len(points) > limit {
		points = points[:limit]
	}

	return points, nil
}

// getReachableTemporaryCheckpoints returns temporary checkpoints from shadow branches
// whose base commit is reachable from the given HEAD hash.
// For default branches, all shadow branches are included.
// For feature branches, only shadow branches whose base commit is in HEAD's history are included.
func getReachableTemporaryCheckpoints(repo *git.Repository, store *checkpoint.GitStore, headHash plumbing.Hash, isOnDefault bool, limit int) []strategy.RewindPoint {
	var points []strategy.RewindPoint

	shadowBranches, _ := store.ListTemporary(context.Background()) //nolint:errcheck // Best-effort
	for _, sb := range shadowBranches {
		// Check if this shadow branch's base commit is reachable from current HEAD
		if !isShadowBranchReachable(repo, sb.BaseCommit, headHash, isOnDefault) {
			continue
		}

		// List checkpoints from this shadow branch
		tempCheckpoints, _ := store.ListCheckpointsForBranch(context.Background(), sb.BranchName, "", limit) //nolint:errcheck // Best-effort
		for _, tc := range tempCheckpoints {
			point := convertTemporaryCheckpoint(repo, tc)
			if point != nil {
				points = append(points, *point)
			}
		}
	}

	return points
}

// isShadowBranchReachable checks if a shadow branch's base commit is reachable from HEAD.
// For default branches, all shadow branches are considered reachable.
// For feature branches, we check if any commit with the base commit prefix is in HEAD's history.
func isShadowBranchReachable(repo *git.Repository, baseCommit string, headHash plumbing.Hash, isOnDefault bool) bool {
	// For default branch: all shadow branches are potentially relevant
	if isOnDefault {
		return true
	}

	// Check if base commit hash prefix matches any commit in HEAD's history
	baseCommitIter, baseErr := repo.Log(&git.LogOptions{From: headHash})
	if baseErr != nil {
		return false
	}
	defer baseCommitIter.Close()

	baseCount := 0
	found := false
	_ = baseCommitIter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort
		baseCount++
		if baseCount > commitScanLimit {
			return errStopIteration
		}
		if strings.HasPrefix(c.Hash.String(), baseCommit) {
			found = true
			return errStopIteration
		}
		return nil
	})

	return found
}

// convertTemporaryCheckpoint converts a TemporaryCheckpointInfo to a RewindPoint.
// Returns nil if the checkpoint should be skipped (no code changes or can't be read).
func convertTemporaryCheckpoint(repo *git.Repository, tc checkpoint.TemporaryCheckpointInfo) *strategy.RewindPoint {
	shadowCommit, commitErr := repo.CommitObject(tc.CommitHash)
	if commitErr != nil {
		return nil
	}

	// Filter out checkpoints with no code changes (only .entire/ metadata changed)
	// This also filters out the first checkpoint which is just a baseline copy
	if !hasCodeChanges(shadowCommit) {
		return nil
	}

	// Read session prompt from the shadow branch commit's tree (not from entire/checkpoints/v1)
	// Temporary checkpoints store their metadata in the shadow branch, not in entire/checkpoints/v1
	var sessionPrompt string
	shadowTree, treeErr := shadowCommit.Tree()
	if treeErr == nil {
		sessionPrompt = strategy.ReadSessionPromptFromTree(shadowTree, tc.MetadataDir)
	}

	return &strategy.RewindPoint{
		ID:               tc.CommitHash.String(),
		Message:          tc.Message,
		MetadataDir:      tc.MetadataDir,
		Date:             tc.Timestamp,
		IsTaskCheckpoint: tc.IsTaskCheckpoint,
		ToolUseID:        tc.ToolUseID,
		SessionID:        tc.SessionID,
		SessionPrompt:    sessionPrompt,
		IsLogsOnly:       false, // Temporary checkpoints can be fully rewound
	}
}

// runExplainBranchWithFilter shows checkpoints on the current branch, optionally filtered by session.
// This is strategy-agnostic - it queries checkpoints directly.
func runExplainBranchWithFilter(w io.Writer, noPager bool, sessionFilter string) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Get current branch name
	branchName := strategy.GetCurrentBranchName(repo)
	if branchName == "" {
		// Detached HEAD state or unborn HEAD - try to use short commit hash if possible
		head, headErr := repo.Head()
		if headErr != nil {
			// Unborn HEAD (no commits yet) - treat as empty history instead of erroring
			if errors.Is(headErr, plumbing.ErrReferenceNotFound) {
				branchName = "HEAD (no commits yet)"
			} else {
				return fmt.Errorf("failed to get HEAD: %w", headErr)
			}
		} else {
			branchName = "HEAD (" + head.Hash().String()[:7] + ")"
		}
	}

	// Get checkpoints for this branch (strategy-agnostic)
	points, err := getBranchCheckpoints(repo, branchCheckpointsLimit)
	if err != nil {
		// Log the error but continue with empty list so user sees helpful message
		logging.Warn(context.Background(), "failed to get branch checkpoints", "error", err)
		points = nil
	}

	// Format output
	output := formatBranchCheckpoints(branchName, points, sessionFilter)

	outputExplainContent(w, output, noPager)
	return nil
}

// runExplainBranchDefault shows all checkpoints on the current branch grouped by date.
// This is a convenience wrapper that calls runExplainBranchWithFilter with no filter.
func runExplainBranchDefault(w io.Writer, noPager bool) error {
	return runExplainBranchWithFilter(w, noPager, "")
}

// outputExplainContent outputs content with optional pager support.
func outputExplainContent(w io.Writer, content string, noPager bool) {
	if noPager {
		fmt.Fprint(w, content)
	} else {
		outputWithPager(w, content)
	}
}

// runExplainCommit looks up the checkpoint associated with a commit.
// Extracts the Entire-Checkpoint trailer and delegates to checkpoint detail view.
// If no trailer found, shows a message indicating no associated checkpoint.
func runExplainCommit(w io.Writer, commitRef string, noPager, verbose, full, searchAll bool) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Resolve the commit reference
	hash, err := repo.ResolveRevision(plumbing.Revision(commitRef))
	if err != nil {
		return fmt.Errorf("commit not found: %s", commitRef)
	}

	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}

	// Extract Entire-Checkpoint trailer
	checkpointID, hasCheckpoint := trailers.ParseCheckpoint(commit.Message)
	if !hasCheckpoint {
		fmt.Fprintln(w, "No associated Entire checkpoint")
		fmt.Fprintf(w, "\nCommit %s does not have an Entire-Checkpoint trailer.\n", hash.String()[:7])
		fmt.Fprintln(w, "This commit was not created during an Entire session, or the trailer was removed.")
		return nil
	}

	// Delegate to checkpoint detail view
	// Note: errW is only used for generate mode, but we pass w for safety
	return runExplainCheckpoint(w, w, checkpointID.String(), noPager, verbose, full, false, false, false, searchAll)
}

// formatSessionInfo formats session information for display.
func formatSessionInfo(session *strategy.Session, sourceRef string, checkpoints []checkpointDetail) string {
	var sb strings.Builder

	// Session header
	fmt.Fprintf(&sb, "Session: %s\n", session.ID)
	fmt.Fprintf(&sb, "Strategy: %s\n", session.Strategy)

	if !session.StartTime.IsZero() {
		fmt.Fprintf(&sb, "Started: %s\n", session.StartTime.Format("2006-01-02 15:04:05"))
	}

	if sourceRef != "" {
		fmt.Fprintf(&sb, "Source Ref: %s\n", sourceRef)
	}

	fmt.Fprintf(&sb, "Checkpoints: %d\n", len(checkpoints))

	// Checkpoint details
	for _, cp := range checkpoints {
		sb.WriteString("\n")

		// Checkpoint header
		taskMarker := ""
		if cp.IsTaskCheckpoint {
			taskMarker = " [Task]"
		}
		fmt.Fprintf(&sb, "─── Checkpoint %d [%s] %s%s ───\n",
			cp.Index, cp.ShortID, cp.Timestamp.Format("2006-01-02 15:04"), taskMarker)
		sb.WriteString("\n")

		// Display all interactions in this checkpoint
		for i, inter := range cp.Interactions {
			// For multiple interactions, add a sub-header
			if len(cp.Interactions) > 1 {
				fmt.Fprintf(&sb, "### Interaction %d\n\n", i+1)
			}

			// Prompt section
			if inter.Prompt != "" {
				sb.WriteString("## Prompt\n\n")
				sb.WriteString(inter.Prompt)
				sb.WriteString("\n\n")
			}

			// Response section
			if len(inter.Responses) > 0 {
				sb.WriteString("## Responses\n\n")
				sb.WriteString(strings.Join(inter.Responses, "\n\n"))
				sb.WriteString("\n\n")
			}

			// Files modified for this interaction
			if len(inter.Files) > 0 {
				fmt.Fprintf(&sb, "Files Modified (%d):\n", len(inter.Files))
				for _, file := range inter.Files {
					fmt.Fprintf(&sb, "  - %s\n", file)
				}
				sb.WriteString("\n")
			}
		}

		// If no interactions, show message and/or files
		if len(cp.Interactions) == 0 {
			// Show commit message as summary when no transcript available
			if cp.Message != "" {
				sb.WriteString(cp.Message)
				sb.WriteString("\n\n")
			}
			// Show aggregate files if available
			if len(cp.Files) > 0 {
				fmt.Fprintf(&sb, "Files Modified (%d):\n", len(cp.Files))
				for _, file := range cp.Files {
					fmt.Fprintf(&sb, "  - %s\n", file)
				}
			}
		}
	}

	return sb.String()
}

// outputWithPager outputs content through a pager if stdout is a terminal and content is long.
func outputWithPager(w io.Writer, content string) {
	// Check if we're writing to stdout and it's a terminal
	if f, ok := w.(*os.File); ok && f == os.Stdout && term.IsTerminal(int(f.Fd())) {
		// Get terminal height
		_, height, err := term.GetSize(int(f.Fd()))
		if err != nil {
			height = 24 // Default fallback
		}

		// Count lines in content
		lineCount := strings.Count(content, "\n")

		// Use pager if content exceeds terminal height
		if lineCount > height-2 {
			pager := os.Getenv("PAGER")
			if pager == "" {
				pager = "less"
			}

			cmd := exec.CommandContext(context.Background(), pager) //nolint:gosec // pager from env is expected
			cmd.Stdin = strings.NewReader(content)
			cmd.Stdout = f
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				// Fallback to direct output if pager fails
				fmt.Fprint(w, content)
			}
			return
		}
	}

	// Direct output for non-terminal or short content
	fmt.Fprint(w, content)
}

// Constants for formatting output
const (
	// maxIntentDisplayLength is the maximum length for intent text before truncation
	maxIntentDisplayLength = 80
	// maxMessageDisplayLength is the maximum length for checkpoint messages before truncation
	maxMessageDisplayLength = 80
	// maxPromptDisplayLength is the maximum length for session prompts before truncation
	maxPromptDisplayLength = 60
	// checkpointIDDisplayLength is the number of characters to show from checkpoint IDs
	checkpointIDDisplayLength = 12
)

// formatBranchCheckpoints formats checkpoint information for a branch.
// Groups commits by checkpoint ID and shows the prompt for each checkpoint.
// If sessionFilter is non-empty, only shows checkpoints matching that session ID (or prefix).
func formatBranchCheckpoints(branchName string, points []strategy.RewindPoint, sessionFilter string) string {
	var sb strings.Builder

	// Branch header
	fmt.Fprintf(&sb, "Branch: %s\n", branchName)

	// Filter by session if specified
	if sessionFilter != "" {
		var filtered []strategy.RewindPoint
		for _, p := range points {
			if p.SessionID == sessionFilter || strings.HasPrefix(p.SessionID, sessionFilter) {
				filtered = append(filtered, p)
			}
		}
		points = filtered
	}

	if len(points) == 0 {
		sb.WriteString("Checkpoints: 0\n")
		if sessionFilter != "" {
			fmt.Fprintf(&sb, "Filtered by session: %s\n", sessionFilter)
		}
		sb.WriteString("\nNo checkpoints found on this branch.\n")
		sb.WriteString("Checkpoints will appear here after you save changes during a Claude session.\n")
		return sb.String()
	}

	// Group by checkpoint ID
	groups := groupByCheckpointID(points)

	fmt.Fprintf(&sb, "Checkpoints: %d\n", len(groups))
	if sessionFilter != "" {
		fmt.Fprintf(&sb, "Filtered by session: %s\n", sessionFilter)
	}
	sb.WriteString("\n")

	// Output each checkpoint group
	for _, group := range groups {
		formatCheckpointGroup(&sb, group)
		sb.WriteString("\n")
	}

	return sb.String()
}

// checkpointGroup represents a group of commits sharing the same checkpoint ID.
type checkpointGroup struct {
	checkpointID string
	prompt       string
	isTemporary  bool // true if any commit is not logs-only (can be rewound)
	isTask       bool // true if this is a task checkpoint
	commits      []commitEntry
}

// commitEntry represents a single git commit within a checkpoint.
type commitEntry struct {
	date    time.Time
	gitSHA  string // short git SHA
	message string
}

// groupByCheckpointID groups rewind points by their checkpoint ID.
// Returns groups sorted by latest commit timestamp (most recent first).
func groupByCheckpointID(points []strategy.RewindPoint) []checkpointGroup {
	if len(points) == 0 {
		return nil
	}

	// Build map of checkpoint ID -> group
	groupMap := make(map[string]*checkpointGroup)
	var order []string // Track insertion order for stable iteration

	for _, point := range points {
		// Determine the checkpoint ID to use for grouping
		cpID := point.CheckpointID.String()
		if cpID == "" {
			// Temporary checkpoints: group by session ID to preserve per-session prompts
			// Use session ID prefix for readability (format: YYYY-MM-DD-uuid)
			cpID = point.SessionID
			if cpID == "" {
				cpID = "temporary" // Fallback if no session ID
			}
		}

		group, exists := groupMap[cpID]
		if !exists {
			group = &checkpointGroup{
				checkpointID: cpID,
				prompt:       point.SessionPrompt,
				isTemporary:  !point.IsLogsOnly,
				isTask:       point.IsTaskCheckpoint,
			}
			groupMap[cpID] = group
			order = append(order, cpID)
		}

		// Short git SHA (7 chars)
		gitSHA := point.ID
		if len(gitSHA) > 7 {
			gitSHA = gitSHA[:7]
		}

		group.commits = append(group.commits, commitEntry{
			date:    point.Date,
			gitSHA:  gitSHA,
			message: point.Message,
		})

		// Update flags - if any commit is temporary/task, the group is too
		if !point.IsLogsOnly {
			group.isTemporary = true
		}
		if point.IsTaskCheckpoint {
			group.isTask = true
		}
		// Update prompt if the group's prompt is empty but this point has one
		if group.prompt == "" && point.SessionPrompt != "" {
			group.prompt = point.SessionPrompt
		}
	}

	// Sort commits within each group by date (most recent first)
	for _, group := range groupMap {
		sort.Slice(group.commits, func(i, j int) bool {
			return group.commits[i].date.After(group.commits[j].date)
		})
	}

	// Build result slice in order, then sort by latest commit
	result := make([]checkpointGroup, 0, len(order))
	for _, cpID := range order {
		result = append(result, *groupMap[cpID])
	}

	// Sort groups by latest commit timestamp (most recent first)
	sort.Slice(result, func(i, j int) bool {
		// Each group's commits are already sorted, so first commit is latest
		if len(result[i].commits) == 0 {
			return false
		}
		if len(result[j].commits) == 0 {
			return true
		}
		return result[i].commits[0].date.After(result[j].commits[0].date)
	})

	return result
}

// formatCheckpointGroup formats a single checkpoint group for display.
func formatCheckpointGroup(sb *strings.Builder, group checkpointGroup) {
	// Checkpoint ID (truncated for display)
	cpID := group.checkpointID
	if len(cpID) > checkpointIDDisplayLength {
		cpID = cpID[:checkpointIDDisplayLength]
	}

	// Build status indicators
	// Skip [temporary] indicator when cpID is already "temporary" to avoid redundancy
	var indicators []string
	if group.isTask {
		indicators = append(indicators, "[Task]")
	}
	if group.isTemporary && cpID != "temporary" {
		indicators = append(indicators, "[temporary]")
	}

	indicatorStr := ""
	if len(indicators) > 0 {
		indicatorStr = " " + strings.Join(indicators, " ")
	}

	// Prompt (truncated)
	var promptStr string
	if group.prompt == "" {
		promptStr = "(no prompt)"
	} else {
		// Quote actual prompts
		promptStr = fmt.Sprintf("%q", strategy.TruncateDescription(group.prompt, maxPromptDisplayLength))
	}

	// Checkpoint header: [checkpoint_id] [indicators] prompt
	fmt.Fprintf(sb, "[%s]%s %s\n", cpID, indicatorStr, promptStr)

	// List commits under this checkpoint
	for _, commit := range group.commits {
		// Format: "  MM-DD HH:MM (git_sha) message"
		dateTimeStr := commit.date.Format("01-02 15:04")
		message := strategy.TruncateDescription(commit.message, maxMessageDisplayLength)
		fmt.Fprintf(sb, "  %s (%s) %s\n", dateTimeStr, commit.gitSHA, message)
	}
}

// countLines counts the number of lines in a byte slice.
// For JSONL content (where each line ends with \n), this returns the line count.
// Empty content returns 0.
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	count := 0
	for _, b := range content {
		if b == '\n' {
			count++
		}
	}
	return count
}

// hasCodeChanges returns true if the commit has changes to non-metadata files.
// Used by getBranchCheckpoints to filter out metadata-only temporary checkpoints.
// Returns false only if the commit has a parent AND only modified .entire/ metadata files.
//
// First commits (no parent) are always considered to have code changes since they
// capture the working copy state at session start - real uncommitted work.
//
// This filters out periodic transcript saves that don't change code.
func hasCodeChanges(commit *object.Commit) bool {
	// First commit on shadow branch captures working copy state - always meaningful
	if commit.NumParents() == 0 {
		return true
	}

	parent, err := commit.Parent(0)
	if err != nil {
		return true // Can't check, assume meaningful
	}

	commitTree, err := commit.Tree()
	if err != nil {
		return true
	}

	parentTree, err := parent.Tree()
	if err != nil {
		return true
	}

	changes, err := parentTree.Diff(commitTree)
	if err != nil {
		return true
	}

	// Check if any non-metadata file was changed
	for _, change := range changes {
		name := change.To.Name
		if name == "" {
			name = change.From.Name
		}
		// Skip .entire/ metadata files
		if !strings.HasPrefix(name, ".entire/") {
			return true
		}
	}

	return false
}
