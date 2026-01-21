package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5/plumbing"
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
	var verboseFlag bool
	var fullFlag bool

	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Explain a session or commit",
		Long: `Explain provides human-readable context about sessions and commits.

Use this command to understand what happened during agent-driven development,
either for self-review or to understand a teammate's work.

By default, explains the current session. Use flags to explain a specific
session or commit.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if Entire is disabled
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			return runExplain(cmd.OutOrStdout(), sessionFlag, commitFlag, checkpointFlag, noPagerFlag, verboseFlag, fullFlag)
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Explain a specific session (ID or prefix)")
	cmd.Flags().StringVar(&commitFlag, "commit", "", "Explain a specific commit (SHA or ref)")
	cmd.Flags().StringVar(&checkpointFlag, "checkpoint", "", "Explain a specific checkpoint (ID or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	cmd.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "Show prompts, files, and session IDs")
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
func runExplainCheckpoint(w io.Writer, checkpointIDPrefix string, noPager, verbose, full bool) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	store := checkpoint.NewGitStore(repo)

	// Find checkpoint by prefix
	committed, err := store.ListCommitted(context.Background())
	if err != nil {
		return fmt.Errorf("failed to list checkpoints: %w", err)
	}

	var fullCheckpointID string
	for _, info := range committed {
		if strings.HasPrefix(info.CheckpointID, checkpointIDPrefix) {
			fullCheckpointID = info.CheckpointID
			break
		}
	}

	if fullCheckpointID == "" {
		return fmt.Errorf("checkpoint not found: %s", checkpointIDPrefix)
	}

	// Load checkpoint data
	result, err := store.ReadCommitted(context.Background(), fullCheckpointID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}

	// Format and output
	output := formatCheckpointOutput(result, fullCheckpointID, verbose, full)

	if noPager {
		fmt.Fprint(w, output)
	} else {
		outputWithPager(w, output)
	}

	return nil
}

// formatCheckpointOutput formats checkpoint data based on verbosity level.
// Default: Summary (ID, session, timestamp, tokens, intent)
// Verbose: + prompts, files, session IDs
// Full: + complete transcript
func formatCheckpointOutput(result *checkpoint.ReadCommittedResult, checkpointID string, verbose, full bool) string {
	var sb strings.Builder
	meta := result.Metadata

	// Header - always shown
	shortID := checkpointID
	if len(shortID) > 12 {
		shortID = shortID[:12]
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
			intent = lines[0]
			if len(intent) > 80 {
				intent = intent[:77] + "..."
			}
		}
	}
	fmt.Fprintf(&sb, "Intent: %s\n", intent)
	sb.WriteString("Outcome: (not generated)\n")

	// Verbose: add files and prompts
	if verbose || full {
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
			sb.WriteString(string(result.Transcript))
			sb.WriteString("\n")
		} else {
			sb.WriteString("  (none)\n")
		}
	}

	return sb.String()
}

// runExplainDefault explains the current session.
func runExplainDefault(w io.Writer, noPager bool) error {
	// Read current session
	currentSessionID, err := paths.ReadCurrentSession()
	if err != nil {
		return fmt.Errorf("failed to read current session: %w", err)
	}

	if currentSessionID == "" {
		return errors.New("no active session. Use --session or --commit to specify what to explain")
	}

	return runExplainSession(w, currentSessionID, noPager)
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
		detail.ShortID = cp.CheckpointID
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

	// Check for Entire metadata
	metadataDir, hasMetadata := paths.ParseMetadataTrailer(commit.Message)
	sessionID, hasSession := paths.ParseSessionTrailer(commit.Message)

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
		HasEntire: hasMetadata || hasSession,
		SessionID: sessionID,
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
	sb.WriteString(fmt.Sprintf("Session: %s\n", session.ID))
	sb.WriteString(fmt.Sprintf("Strategy: %s\n", session.Strategy))

	if !session.StartTime.IsZero() {
		sb.WriteString(fmt.Sprintf("Started: %s\n", session.StartTime.Format("2006-01-02 15:04:05")))
	}

	if sourceRef != "" {
		sb.WriteString(fmt.Sprintf("Source Ref: %s\n", sourceRef))
	}

	sb.WriteString(fmt.Sprintf("Checkpoints: %d\n", len(checkpoints)))

	// Checkpoint details
	for _, cp := range checkpoints {
		sb.WriteString("\n")

		// Checkpoint header
		taskMarker := ""
		if cp.IsTaskCheckpoint {
			taskMarker = " [Task]"
		}
		sb.WriteString(fmt.Sprintf("─── Checkpoint %d [%s] %s%s ───\n",
			cp.Index, cp.ShortID, cp.Timestamp.Format("2006-01-02 15:04"), taskMarker))
		sb.WriteString("\n")

		// Display all interactions in this checkpoint
		for i, inter := range cp.Interactions {
			// For multiple interactions, add a sub-header
			if len(cp.Interactions) > 1 {
				sb.WriteString(fmt.Sprintf("### Interaction %d\n\n", i+1))
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
				sb.WriteString(fmt.Sprintf("Files Modified (%d):\n", len(inter.Files)))
				for _, file := range inter.Files {
					sb.WriteString(fmt.Sprintf("  - %s\n", file))
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
				sb.WriteString(fmt.Sprintf("Files Modified (%d):\n", len(cp.Files)))
				for _, file := range cp.Files {
					sb.WriteString(fmt.Sprintf("  - %s\n", file))
				}
			}
		}
	}

	return sb.String()
}

// formatCommitInfo formats commit information for display.
func formatCommitInfo(info *commitInfo) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Commit: %s (%s)\n", info.SHA, info.ShortSHA))
	sb.WriteString(fmt.Sprintf("Date: %s\n", info.Date.Format("2006-01-02 15:04:05")))

	if info.HasEntire && info.SessionID != "" {
		sb.WriteString(fmt.Sprintf("Session: %s\n", info.SessionID))
	}

	sb.WriteString("\n")

	// Message
	sb.WriteString("Message:\n")
	sb.WriteString(fmt.Sprintf("  %s\n", info.Message))
	sb.WriteString("\n")

	// Files modified
	if len(info.Files) > 0 {
		sb.WriteString(fmt.Sprintf("Files Modified (%d):\n", len(info.Files)))
		for _, file := range info.Files {
			sb.WriteString(fmt.Sprintf("  - %s\n", file))
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
