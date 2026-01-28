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

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/strategy"
	"entire.io/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// commitInfo holds information about a commit for display purposes.
type commitInfo struct {
	SHA       string
	ShortSHA  string
	Message   string
	Author    string
	Email     string
	Date      time.Time
	Files     []string
	HasEntire bool
	SessionID string
}

// interaction holds a single prompt and its responses for display.
type interaction struct {
	Prompt    string
	Responses []string // Multiple responses can occur between tool calls
	Files     []string
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

	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Explain a session, commit, or checkpoint",
		Long: `Explain provides human-readable context about sessions, commits, and checkpoints.

Use this command to understand what happened during agent-driven development,
either for self-review or to understand a teammate's work.

By default, explains the current session. Use flags to explain a specific
session, commit, or checkpoint.

Output verbosity levels (for --checkpoint):
  Default:   Detailed view (ID, session, timestamp, tokens, intent, prompts, files)
  --short:   Summary only (ID, session, timestamp, tokens, intent)
  --full:    + complete transcript

Only one of --session, --commit, or --checkpoint can be specified at a time.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if Entire is disabled
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			// Convert short flag to verbose (verbose = !short)
			verbose := !shortFlag
			return runExplain(cmd.OutOrStdout(), sessionFlag, commitFlag, checkpointFlag, noPagerFlag, verbose, fullFlag)
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Explain a specific session (ID or prefix)")
	cmd.Flags().StringVar(&commitFlag, "commit", "", "Explain a specific commit (SHA or ref)")
	cmd.Flags().StringVarP(&checkpointFlag, "checkpoint", "c", "", "Explain a specific checkpoint (ID or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	cmd.Flags().BoolVarP(&shortFlag, "short", "s", false, "Show summary only (omit prompts and files)")
	cmd.Flags().BoolVar(&fullFlag, "full", false, "Show complete transcript")

	return cmd
}

// runExplain routes to the appropriate explain function based on flags.
func runExplain(w io.Writer, sessionID, commitRef, checkpointID string, noPager, verbose, full bool) error {
	// Count mutually exclusive flags
	flagCount := 0
	if sessionID != "" {
		flagCount++
	}
	if commitRef != "" {
		flagCount++
	}
	if checkpointID != "" {
		flagCount++
	}
	if flagCount > 1 {
		return errors.New("cannot specify multiple of --session, --commit, --checkpoint")
	}

	// Route to appropriate handler
	if sessionID != "" {
		return runExplainSession(w, sessionID, noPager)
	}
	if commitRef != "" {
		return runExplainCommit(w, commitRef)
	}
	if checkpointID != "" {
		return runExplainCheckpoint(w, checkpointID, noPager, verbose, full)
	}

	// Default: explain current session
	return runExplainDefault(w, noPager)
}

// runExplainCheckpoint explains a specific checkpoint.
// Supports both committed checkpoints (by checkpoint ID) and temporary checkpoints (by git SHA).
// First tries to match committed checkpoints, then falls back to temporary checkpoints.
func runExplainCheckpoint(w io.Writer, checkpointIDPrefix string, noPager, verbose, full bool) error {
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

	var fullCheckpointID id.CheckpointID
	for _, info := range committed {
		if strings.HasPrefix(info.CheckpointID.String(), checkpointIDPrefix) {
			fullCheckpointID = info.CheckpointID
			break
		}
	}

	// If not found in committed, try temporary checkpoints by git SHA
	if fullCheckpointID.IsEmpty() {
		output, found := explainTemporaryCheckpoint(repo, store, checkpointIDPrefix, verbose, full)
		if found {
			outputExplainContent(w, output, noPager)
			return nil
		}
		return fmt.Errorf("checkpoint not found: %s", checkpointIDPrefix)
	}

	// Load checkpoint data
	result, err := store.ReadCommitted(context.Background(), fullCheckpointID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if result == nil {
		return fmt.Errorf("checkpoint not found: %s", fullCheckpointID)
	}

	// Look up the commit message for this checkpoint
	commitMessage := findCommitMessageForCheckpoint(repo, fullCheckpointID)

	// Format and output
	output := formatCheckpointOutput(result, fullCheckpointID, commitMessage, verbose, full)
	outputExplainContent(w, output, noPager)
	return nil
}

// explainTemporaryCheckpoint finds and formats a temporary checkpoint by shadow commit hash prefix.
// Returns the formatted output and whether the checkpoint was found.
func explainTemporaryCheckpoint(repo *git.Repository, store *checkpoint.GitStore, shaPrefix string, verbose, full bool) (string, bool) {
	// Get current HEAD to find shadow branch
	head, err := repo.Head()
	if err != nil {
		return "", false
	}
	headShort := head.Hash().String()[:7]

	// List temporary checkpoints on current shadow branch
	tempCheckpoints, err := store.ListTemporaryCheckpoints(context.Background(), headShort, "", branchCheckpointsLimit)
	if err != nil {
		return "", false
	}

	// Find checkpoint matching the SHA prefix
	for _, tc := range tempCheckpoints {
		if strings.HasPrefix(tc.CommitHash.String(), shaPrefix) {
			// Found it - read metadata from shadow branch commit tree
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
					intent = truncateString(lines[0], maxIntentDisplayLength)
				}
			}
			fmt.Fprintf(&sb, "Intent: %s\n", intent)
			sb.WriteString("Outcome: (not generated)\n")

			// Verbose: show prompts
			if verbose || full {
				sb.WriteString("\n")
				sb.WriteString("Prompts:\n")
				if sessionPrompt != "" {
					sb.WriteString(sessionPrompt)
					sb.WriteString("\n")
				} else {
					sb.WriteString("  (none)\n")
				}
			}

			// Full: show transcript
			if full {
				// Try to read transcript from shadow branch
				transcriptPath := tc.MetadataDir + "/full.jsonl"
				if transcriptFile, fileErr := shadowTree.File(transcriptPath); fileErr == nil {
					if content, readErr := transcriptFile.Contents(); readErr == nil {
						sb.WriteString("\n")
						sb.WriteString("Transcript:\n")
						sb.WriteString(content)
						sb.WriteString("\n")
					}
				}
			}

			return sb.String(), true
		}
	}

	return "", false
}

// findCommitMessageForCheckpoint searches git history for a commit with the
// Entire-Checkpoint trailer matching the given checkpoint ID, and returns
// the first line of the commit message. Returns empty string if not found.
func findCommitMessageForCheckpoint(repo *git.Repository, checkpointID id.CheckpointID) string {
	// Get HEAD reference
	head, err := repo.Head()
	if err != nil {
		return ""
	}

	// Iterate through commit history (limit to recent commits for performance)
	commitIter, err := repo.Log(&git.LogOptions{
		From: head.Hash(),
	})
	if err != nil {
		return ""
	}
	defer commitIter.Close()

	count := 0

	for {
		commit, iterErr := commitIter.Next()
		if iterErr != nil {
			break
		}
		count++
		if count > commitScanLimit {
			break
		}

		// Check if this commit has our checkpoint ID
		foundID, hasTrailer := trailers.ParseCheckpoint(commit.Message)
		if hasTrailer && foundID == checkpointID {
			// Return first line of commit message (without trailing newline)
			firstLine := strings.Split(commit.Message, "\n")[0]
			return strings.TrimSpace(firstLine)
		}
	}

	return ""
}

// formatCheckpointOutput formats checkpoint data based on verbosity level.
// When verbose is false: summary only (ID, session, timestamp, tokens, intent).
// When verbose is true: adds prompts, files, and commit message details.
// When full is true: includes the complete transcript in addition to verbose details.
func formatCheckpointOutput(result *checkpoint.ReadCommittedResult, checkpointID id.CheckpointID, commitMessage string, verbose, full bool) string {
	var sb strings.Builder
	meta := result.Metadata

	// Header - always shown
	shortID := checkpointID
	if len(shortID) > checkpointIDDisplayLength {
		shortID = shortID[:checkpointIDDisplayLength]
	}
	fmt.Fprintf(&sb, "Checkpoint: %s\n", shortID)
	fmt.Fprintf(&sb, "Session: %s\n", meta.SessionID)
	fmt.Fprintf(&sb, "Created: %s\n", meta.CreatedAt.Format("2006-01-02 15:04:05"))

	// Token usage
	if meta.TokenUsage != nil {
		totalTokens := meta.TokenUsage.InputTokens + meta.TokenUsage.CacheCreationTokens +
			meta.TokenUsage.CacheReadTokens + meta.TokenUsage.OutputTokens
		fmt.Fprintf(&sb, "Tokens: %d\n", totalTokens)
	}

	sb.WriteString("\n")

	// Intent (use first line of prompts as fallback until AI summary is available)
	intent := "(not generated)"
	if result.Prompts != "" {
		lines := strings.Split(result.Prompts, "\n")
		if len(lines) > 0 && lines[0] != "" {
			intent = truncateString(lines[0], maxIntentDisplayLength)
		}
	}
	fmt.Fprintf(&sb, "Intent: %s\n", intent)
	sb.WriteString("Outcome: (not generated)\n")

	// Verbose: add commit message, files, and prompts
	if verbose || full {
		// Commit message section (only if available)
		if commitMessage != "" {
			sb.WriteString("\n")
			fmt.Fprintf(&sb, "Commit: %s\n", commitMessage)
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

		sb.WriteString("\n")

		// Prompts section
		sb.WriteString("Prompts:\n")
		if result.Prompts != "" {
			sb.WriteString(result.Prompts)
			sb.WriteString("\n")
		} else {
			sb.WriteString("  (none)\n")
		}
	}

	// Full: add transcript
	if full {
		sb.WriteString("\n")
		sb.WriteString("Transcript:\n")
		if len(result.Transcript) > 0 {
			sb.Write(result.Transcript)
			sb.WriteString("\n")
		} else {
			sb.WriteString("  (none)\n")
		}
	}

	return sb.String()
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
//   - Includes both committed checkpoints (entire/sessions) and temporary checkpoints (shadow branches)
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
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Check if we're on the default branch
	isOnDefault, _, _ := IsOnDefaultBranch() //nolint:errcheck // Best-effort, defaults to false
	var mainBranchHash plumbing.Hash
	if !isOnDefault {
		mainBranchHash = strategy.GetMainBranchHash(repo)
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

	// Fetch metadata branch tree once (used for reading session prompts)
	metadataTree, _ := strategy.GetMetadataBranchTree(repo) //nolint:errcheck // Best-effort, continue without prompts

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
		if mainBranchHash != plumbing.ZeroHash {
			if isAncestorOf(repo, c.Hash, mainBranchHash) {
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
			ID:           c.Hash.String(),
			Message:      message,
			Date:         c.Author.When,
			IsLogsOnly:   true, // Committed checkpoints are logs-only
			CheckpointID: cpID,
			SessionID:    cpInfo.SessionID,
		}

		// Read session prompt from metadata branch (best-effort)
		if metadataTree != nil {
			point.SessionPrompt = strategy.ReadSessionPromptFromTree(metadataTree, cpID.Path())
		}

		points = append(points, point)
		return nil
	})

	if err != nil && !errors.Is(err, errStopIteration) {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	// Also get temporary checkpoints from shadow branch for current HEAD
	headShort := head.Hash().String()[:7]
	tempCheckpoints, _ := store.ListTemporaryCheckpoints(context.Background(), headShort, "", limit) //nolint:errcheck // Best-effort, continue without temp checkpoints
	for _, tc := range tempCheckpoints {
		shadowCommit, commitErr := repo.CommitObject(tc.CommitHash)
		if commitErr != nil {
			continue
		}

		// Filter out checkpoints with no code changes (only .entire/ metadata changed)
		// This also filters out the first checkpoint which is just a baseline copy
		if !hasCodeChanges(shadowCommit) {
			continue
		}

		// Read session prompt from the shadow branch commit's tree (not from entire/sessions)
		// Temporary checkpoints store their metadata in the shadow branch, not in entire/sessions
		var sessionPrompt string
		shadowTree, treeErr := shadowCommit.Tree()
		if treeErr == nil {
			sessionPrompt = strategy.ReadSessionPromptFromTree(shadowTree, tc.MetadataDir)
		}

		points = append(points, strategy.RewindPoint{
			ID:               tc.CommitHash.String(),
			Message:          tc.Message,
			MetadataDir:      tc.MetadataDir,
			Date:             tc.Timestamp,
			IsTaskCheckpoint: tc.IsTaskCheckpoint,
			ToolUseID:        tc.ToolUseID,
			SessionID:        tc.SessionID,
			SessionPrompt:    sessionPrompt,
			IsLogsOnly:       false, // Temporary checkpoints can be fully rewound
		})
	}

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

// isAncestorOf checks if commit is an ancestor of (or equal to) target.
// Returns true if target can reach commit by following parent links.
func isAncestorOf(repo *git.Repository, commit, target plumbing.Hash) bool {
	if commit == target {
		return true
	}

	iter, err := repo.Log(&git.LogOptions{From: target})
	if err != nil {
		return false
	}
	defer iter.Close()

	found := false
	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are non-fatal
		count++
		if count > 1000 {
			return errStopIteration
		}
		if c.Hash == commit {
			found = true
			return errStopIteration
		}
		return nil
	})

	return found
}

// runExplainBranchDefault shows all checkpoints on the current branch grouped by date.
// This is strategy-agnostic - it queries checkpoints directly.
func runExplainBranchDefault(w io.Writer, noPager bool) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Get current branch name
	branchName := strategy.GetCurrentBranchName(repo)
	if branchName == "" {
		// Detached HEAD state - use short commit hash instead
		head, headErr := repo.Head()
		if headErr != nil {
			return fmt.Errorf("failed to get HEAD: %w", headErr)
		}
		branchName = "HEAD (" + head.Hash().String()[:7] + ")"
	}

	// Get checkpoints for this branch (strategy-agnostic)
	points, err := getBranchCheckpoints(repo, branchCheckpointsLimit)
	if err != nil {
		// Log error but continue with empty list
		points = nil
	}

	// Format output
	output := formatBranchCheckpoints(branchName, points)

	outputExplainContent(w, output, noPager)
	return nil
}

// outputExplainContent outputs content with optional pager support.
func outputExplainContent(w io.Writer, content string, noPager bool) {
	if noPager {
		fmt.Fprint(w, content)
	} else {
		outputWithPager(w, content)
	}
}

// runExplainSession explains a specific session.
func runExplainSession(w io.Writer, sessionID string, noPager bool) error {
	strat := GetStrategy()

	// Get session details
	session, err := strategy.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, strategy.ErrNoSession) {
			return fmt.Errorf("session not found: %s", sessionID)
		}
		return fmt.Errorf("failed to get session: %w", err)
	}

	// Get source ref (metadata branch + commit) for this session
	sourceRef := strat.GetSessionMetadataRef(session.ID)

	// Gather checkpoint details
	checkpointDetails := gatherCheckpointDetails(strat, session)

	// For strategies like shadow where active sessions may not have checkpoints,
	// try to get the current session transcript directly
	if len(checkpointDetails) == 0 && len(session.Checkpoints) == 0 {
		checkpointDetails = gatherCurrentSessionDetails(strat, session)
	}

	// Format output
	output := formatSessionInfo(session, sourceRef, checkpointDetails)

	// Output with pager if appropriate
	if noPager {
		fmt.Fprint(w, output)
	} else {
		outputWithPager(w, output)
	}

	return nil
}

// gatherCurrentSessionDetails attempts to get transcript info for sessions without checkpoints.
// This handles strategies like shadow where active sessions may not have checkpoint commits.
func gatherCurrentSessionDetails(strat strategy.Strategy, session *strategy.Session) []checkpointDetail {
	// Try to get transcript via GetSessionContext which reads from metadata branch
	// For shadow, we can read the transcript from the same location pattern
	contextContent := strat.GetSessionContext(session.ID)
	if contextContent == "" {
		return nil
	}

	// Parse the context.md to extract the last prompt/summary
	// Context.md typically has sections like "# Prompt\n...\n## Summary\n..."
	detail := checkpointDetail{
		Index:     1,
		Timestamp: session.StartTime,
		Message:   "Current session",
	}

	// Try to extract prompt and summary from context.md
	lines := strings.Split(contextContent, "\n")
	var inPrompt, inSummary bool
	var promptLines, summaryLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") && strings.Contains(strings.ToLower(trimmed), "prompt") {
			inPrompt = true
			inSummary = false
			continue
		}
		if strings.HasPrefix(trimmed, "## ") && strings.Contains(strings.ToLower(trimmed), "summary") {
			inPrompt = false
			inSummary = true
			continue
		}
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "# ") {
			inPrompt = false
			inSummary = false
			continue
		}

		if inPrompt {
			promptLines = append(promptLines, line)
		} else if inSummary {
			summaryLines = append(summaryLines, line)
		}
	}

	var inter interaction
	if len(promptLines) > 0 {
		inter.Prompt = strings.TrimSpace(strings.Join(promptLines, "\n"))
	}
	if len(summaryLines) > 0 {
		inter.Responses = []string{strings.TrimSpace(strings.Join(summaryLines, "\n"))}
	}

	// If we couldn't parse structured content, show the raw context
	if inter.Prompt == "" && len(inter.Responses) == 0 {
		inter.Responses = []string{contextContent}
	}

	if inter.Prompt != "" || len(inter.Responses) > 0 {
		detail.Interactions = []interaction{inter}
	}

	return []checkpointDetail{detail}
}

// gatherCheckpointDetails extracts detailed information for each checkpoint.
// Checkpoints come in newest-first order, but we number them oldest=1, newest=N.
func gatherCheckpointDetails(strat strategy.Strategy, session *strategy.Session) []checkpointDetail {
	details := make([]checkpointDetail, 0, len(session.Checkpoints))
	total := len(session.Checkpoints)

	for i, cp := range session.Checkpoints {
		detail := checkpointDetail{
			Index:            total - i, // Reverse numbering: oldest=1, newest=N
			Timestamp:        cp.Timestamp,
			IsTaskCheckpoint: cp.IsTaskCheckpoint,
			Message:          cp.Message,
		}

		// Use checkpoint ID for display (truncate long IDs)
		detail.ShortID = cp.CheckpointID.String()
		if len(detail.ShortID) > 12 {
			detail.ShortID = detail.ShortID[:12]
		}

		// Try to get transcript for this checkpoint
		transcriptContent, err := strat.GetCheckpointLog(cp)
		if err == nil {
			transcript, parseErr := parseTranscriptFromBytes(transcriptContent)
			if parseErr == nil {
				// Extract all prompt/response pairs from the transcript
				pairs := ExtractAllPromptResponses(transcript)
				for _, pair := range pairs {
					detail.Interactions = append(detail.Interactions, interaction(pair))
				}

				// Aggregate all files for the checkpoint
				fileSet := make(map[string]bool)
				for _, pair := range pairs {
					for _, f := range pair.Files {
						if !fileSet[f] {
							fileSet[f] = true
							detail.Files = append(detail.Files, f)
						}
					}
				}
			}
		}

		details = append(details, detail)
	}

	return details
}

// runExplainCommit explains a specific commit.
func runExplainCommit(w io.Writer, commitRef string) error {
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

	// Get files changed in this commit (diff from parent to current)
	var files []string
	commitTree, err := commit.Tree()
	if err == nil && commit.NumParents() > 0 {
		parent, parentErr := commit.Parent(0)
		if parentErr == nil {
			parentTree, treeErr := parent.Tree()
			if treeErr == nil {
				// Diff from parent to current commit to show what changed
				changes, diffErr := parentTree.Diff(commitTree)
				if diffErr == nil {
					for _, change := range changes {
						name := change.To.Name
						if name == "" {
							name = change.From.Name
						}
						files = append(files, name)
					}
				}
			}
		}
	}

	// Check for Entire metadata - try multiple trailer types
	metadataDir, hasMetadata := trailers.ParseMetadata(commit.Message)
	sessionID, hasSession := trailers.ParseSession(commit.Message)
	checkpointID, hasCheckpoint := trailers.ParseCheckpoint(commit.Message)

	// If no session trailer, try to extract from metadata path.
	// Note: extractSessionIDFromMetadata is defined in rewind.go as it's used
	// by both the rewind and explain commands for parsing metadata paths.
	if !hasSession && hasMetadata {
		sessionID = extractSessionIDFromMetadata(metadataDir)
		hasSession = sessionID != ""
	}

	// Build commit info
	fullSHA := hash.String()
	shortSHA := fullSHA
	if len(fullSHA) >= 7 {
		shortSHA = fullSHA[:7]
	}

	info := &commitInfo{
		SHA:       fullSHA,
		ShortSHA:  shortSHA,
		Message:   strings.Split(commit.Message, "\n")[0], // First line only
		Author:    commit.Author.Name,
		Email:     commit.Author.Email,
		Date:      commit.Author.When,
		Files:     files,
		HasEntire: hasMetadata || hasSession || hasCheckpoint,
		SessionID: sessionID,
	}

	// If we have a checkpoint ID but no session, try to look up session from metadata
	if hasCheckpoint && sessionID == "" {
		if result, err := checkpoint.NewGitStore(repo).ReadCommitted(context.Background(), checkpointID); err == nil {
			info.SessionID = result.Metadata.SessionID
		}
	}

	// Format and output
	output := formatCommitInfo(info)
	fmt.Fprint(w, output)

	return nil
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

// formatCommitInfo formats commit information for display.
func formatCommitInfo(info *commitInfo) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Commit: %s (%s)\n", info.SHA, info.ShortSHA)
	fmt.Fprintf(&sb, "Date: %s\n", info.Date.Format("2006-01-02 15:04:05"))

	if info.HasEntire && info.SessionID != "" {
		fmt.Fprintf(&sb, "Session: %s\n", info.SessionID)
	}

	sb.WriteString("\n")

	// Message
	sb.WriteString("Message:\n")
	fmt.Fprintf(&sb, "  %s\n", info.Message)
	sb.WriteString("\n")

	// Files modified
	if len(info.Files) > 0 {
		fmt.Fprintf(&sb, "Files Modified (%d):\n", len(info.Files))
		for _, file := range info.Files {
			fmt.Fprintf(&sb, "  - %s\n", file)
		}
		sb.WriteString("\n")
	}

	// Note for non-Entire commits
	if !info.HasEntire {
		sb.WriteString("Note: No Entire session data available for this commit.\n")
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
func formatBranchCheckpoints(branchName string, points []strategy.RewindPoint) string {
	var sb strings.Builder

	// Branch header
	fmt.Fprintf(&sb, "Branch: %s\n", branchName)

	if len(points) == 0 {
		sb.WriteString("Checkpoints: 0\n")
		sb.WriteString("\nNo checkpoints found on this branch.\n")
		sb.WriteString("Checkpoints will appear here after you save changes during a Claude session.\n")
		return sb.String()
	}

	// Group by checkpoint ID
	groups := groupByCheckpointID(points)

	fmt.Fprintf(&sb, "Checkpoints: %d\n", len(groups))
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
		// Determine the checkpoint ID to use
		cpID := point.CheckpointID.String()
		if cpID == "" {
			// All temporary checkpoints group together under "temporary"
			cpID = "temporary"
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

		// Update flags - if any commit is temporary, the group is temporary
		if !point.IsLogsOnly {
			group.isTemporary = true
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
		indicators = append(indicators, "[task]")
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
		promptStr = fmt.Sprintf("%q", truncateString(group.prompt, maxPromptDisplayLength))
	}

	// Checkpoint header: [checkpoint_id] [indicators] prompt
	fmt.Fprintf(sb, "[%s]%s %s\n", cpID, indicatorStr, promptStr)

	// List commits under this checkpoint
	for _, commit := range group.commits {
		// Format: "  MM-DD HH:MM (git_sha) message"
		dateTimeStr := commit.date.Format("01-02 15:04")
		message := truncateString(commit.message, maxMessageDisplayLength)
		fmt.Fprintf(sb, "  %s (%s) %s\n", dateTimeStr, commit.gitSHA, message)
	}
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

// truncateString truncates a string to the specified length, adding "..." if truncated.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
