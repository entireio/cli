package cli

import (
	"context"
	"encoding/json"
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

	git "github.com/go-git/go-git/v5"
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

// checkpointWithMeta holds a rewind point together with its loaded metadata.
// Used by formatBranchExplain to display metadata summaries when available.
type checkpointWithMeta struct {
	Point            strategy.RewindPoint
	Metadata         *checkpoint.CommittedMetadata
	GeneratedSummary *Summary // Populated by --generate when no stored summary exists
	UsedHeuristic    bool     // True if GeneratedSummary was created via heuristic fallback
	Transcript       string   // Populated for --full mode
	CommitMessage    string   // Fallback: git commit message(s) associated with this checkpoint
}

func newExplainCmd() *cobra.Command {
	var sessionFlag string
	var commitFlag string
	var checkpointFlag string
	var noPagerFlag bool
	var verboseFlag bool
	var fullFlag bool
	var generateFlag bool
	var forceFlag bool
	var limitFlag int

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

			return runExplain(cmd.OutOrStdout(), sessionFlag, commitFlag, checkpointFlag, noPagerFlag, verboseFlag, fullFlag, generateFlag, forceFlag, limitFlag)
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Explain a specific session (ID or prefix)")
	cmd.Flags().StringVar(&commitFlag, "commit", "", "Explain a specific commit (SHA or ref)")
	cmd.Flags().StringVar(&checkpointFlag, "checkpoint", "", "Generate summary for a single checkpoint (ID or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	cmd.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "Show prompts, files, and session IDs")
	cmd.Flags().BoolVar(&fullFlag, "full", false, "Show complete transcript")
	cmd.Flags().BoolVar(&generateFlag, "generate", false, "Generate AI summaries for checkpoints")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Force regeneration of existing summaries (use with --generate)")
	cmd.Flags().IntVar(&limitFlag, "limit", 0, "Limit number of checkpoints shown (0 = auto)")

	return cmd
}

// runExplain routes to the appropriate explain function based on flags.
// The verbose and full parameters are accepted for future use.
func runExplain(w io.Writer, sessionID, commitRef, checkpointID string, noPager, verbose, full, generate, force bool, limit int) error {
	// Silence unused variable warnings until verbose/full are implemented
	_ = verbose
	_ = full

	// Error if multiple mutually-exclusive flags are provided
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
		return runExplainCheckpoint(w, checkpointID, generate)
	}

	// Default: explain branch-level view
	return runExplainDefault(w, noPager, verbose, full, generate, force, limit)
}

// defaultLimitOnMain is the default number of checkpoints to show on main/master branches.
const defaultLimitOnMain = 10

// notGeneratedPlaceholder is the placeholder text for missing summaries.
const notGeneratedPlaceholder = "(not generated)"

// runExplainDefault explains the current branch state.
// Shows branch-level information with checkpoint listing.
// The verbose, full, generate, and force parameters control output detail level.
// When force is true, regenerates summaries even if they already exist.
func runExplainDefault(w io.Writer, noPager, verbose, full, generate, force bool, limit int) error {
	// Get current branch info
	isDefault, branchName, err := IsOnDefaultBranch()
	if err != nil {
		branchName = "unknown"
	}

	// Apply default limit on main branch when limit is 0 (auto)
	effectiveLimit := limit
	if isDefault && limit == 0 {
		effectiveLimit = defaultLimitOnMain
	}

	strat := GetStrategy()

	// Get all rewind points (uncommitted checkpoints on shadow branches)
	allPoints, err := strat.GetRewindPoints(0)
	if err != nil {
		allPoints = nil
	}

	// Map of checkpoint ID -> commit message (for fallback display)
	var commitMessages map[string]string

	// If no uncommitted rewind points, also check for committed checkpoints
	// This shows the development history after work has been committed
	if len(allPoints) == 0 {
		allPoints, commitMessages = getCommittedCheckpointsAsRewindPoints()
	}

	totalCount := len(allPoints)

	// Apply limit
	points := allPoints
	if effectiveLimit > 0 && len(points) > effectiveLimit {
		points = points[:effectiveLimit]
	}

	// Track token usage when generating summaries
	var totalInputTokens, totalOutputTokens int
	var generatedCount, fallbackCount int

	// Collect summaries to save (batched at the end to reduce commits)
	var summariesToSave []checkpoint.UpdateSummaryOptions

	// Load metadata for each checkpoint
	checkpoints := make([]checkpointWithMeta, len(points))
	for i, point := range points {
		meta := loadCheckpointMetadata(point.CheckpointID)
		checkpoints[i] = checkpointWithMeta{
			Point:         point,
			Metadata:      meta,
			CommitMessage: commitMessages[point.CheckpointID], // Fallback if no summary
		}

		// If --generate and (no stored summary OR --force), generate one from transcript and save it
		needsGeneration := meta == nil || meta.Intent == "" || force
		if generate && needsGeneration {
			shortID := point.CheckpointID
			if len(shortID) > 12 {
				shortID = shortID[:12]
			}
			fmt.Fprintf(os.Stderr, "Generating summary for checkpoint %s...", shortID)

			result := generateSummaryForCheckpointWithUsage(point.CheckpointID)
			if result != nil {
				checkpoints[i].GeneratedSummary = result.Summary
				checkpoints[i].UsedHeuristic = result.UsedFallback
				totalInputTokens += result.InputTokens
				totalOutputTokens += result.OutputTokens
				generatedCount++

				if result.UsedFallback {
					fallbackCount++
					fmt.Fprintf(os.Stderr, " (using heuristic)\n")
				} else {
					fmt.Fprintf(os.Stderr, " done\n")
				}

				// Queue summary for batch save (only if non-empty)
				if result.Summary != nil && (result.Summary.Intent != "" || result.Summary.Outcome != "") {
					source := checkpoint.SummarySourceAI
					if result.UsedFallback {
						source = checkpoint.SummarySourceHeuristic
					}
					summariesToSave = append(summariesToSave, checkpoint.UpdateSummaryOptions{
						CheckpointID:   point.CheckpointID,
						Intent:         result.Summary.Intent,
						Outcome:        result.Summary.Outcome,
						Learnings:      result.Summary.Learnings,
						FrictionPoints: result.Summary.FrictionPoints,
						SummarySource:  source,
						AuthorName:     "Entire CLI",
						AuthorEmail:    "cli@entire.io",
					})
				}
			} else {
				fmt.Fprintf(os.Stderr, " failed\n")
			}
		}

		// If --full or --verbose, load the transcript content
		if full || verbose {
			checkpoints[i].Transcript = loadCheckpointTranscript(point.CheckpointID)
		}
	}

	// Generate branch-level summary if --generate and we have checkpoints with intents
	var branchSummary *Summary
	var branchSummaryToSave *checkpoint.BranchSummary
	if generate && len(checkpoints) > 0 {
		fmt.Fprintf(os.Stderr, "Generating branch summary...\n")
		result := generateBranchSummaryFromCheckpointsWithUsage(checkpoints)
		if result != nil {
			branchSummary = result.Summary
			totalInputTokens += result.InputTokens
			totalOutputTokens += result.OutputTokens

			// Prepare branch summary for persistence
			branchSummaryToSave = &checkpoint.BranchSummary{
				BranchName:      branchName,
				Intent:          result.Summary.Intent,
				Outcome:         result.Summary.Outcome,
				HeadCommit:      getCurrentHeadShort(),
				Agent:           "Entire CLI",
				CheckpointCount: len(checkpoints),
				CheckpointIDs:   extractCheckpointIDs(checkpoints),
			}
		}
	}

	// Batch save all generated summaries and branch summary in a single commit
	if len(summariesToSave) > 0 || branchSummaryToSave != nil {
		batchSaveSummariesWithBranch(summariesToSave, branchSummaryToSave)
	}

	// Show generation stats
	if generate && generatedCount > 0 {
		switch {
		case fallbackCount > 0 && fallbackCount == generatedCount:
			fmt.Fprintf(os.Stderr, "Generated %d summary(ies) using heuristic (Claude CLI unavailable)\n\n", generatedCount)
		case fallbackCount > 0:
			fmt.Fprintf(os.Stderr, "Generated %d summary(ies) (%d using heuristic fallback)\n\n", generatedCount, fallbackCount)
		default:
			fmt.Fprintf(os.Stderr, "Generated %d summary(ies)\n\n", generatedCount)
		}
	} else if generate && len(points) > 0 {
		fmt.Fprintf(os.Stderr, "No checkpoints needed summary generation\n\n")
	}

	// Format output
	output := formatBranchExplain(branchName, checkpoints, branchSummary, verbose, full, isDefault, effectiveLimit, totalCount)

	// Append token usage if any summarization was done
	if generate && (totalInputTokens > 0 || totalOutputTokens > 0) {
		output += fmt.Sprintf("\nToken usage: %d input, %d output\n", totalInputTokens, totalOutputTokens)
	}

	if noPager {
		fmt.Fprint(w, output)
	} else {
		outputWithPager(w, output)
	}

	return nil
}

// getCommittedCheckpointsAsRewindPoints loads committed checkpoints from entire/sessions
// and converts them to RewindPoint format for display.
// Only returns checkpoints that are associated with commits on the current branch
// (by looking at Entire-Checkpoint trailers on code commits).
// Also returns a map of checkpoint ID -> commit message for fallback display.
func getCommittedCheckpointsAsRewindPoints() ([]strategy.RewindPoint, map[string]string) {
	repo, err := openRepository()
	if err != nil {
		return nil, nil
	}

	// Get checkpoint IDs and commit messages from Entire-Checkpoint trailers
	checkpointMessages := getBranchCheckpointInfo(repo)

	store := checkpoint.NewGitStore(repo)
	committed, err := store.ListCommitted(context.Background())
	if err != nil {
		return nil, checkpointMessages
	}

	points := make([]strategy.RewindPoint, 0, len(committed))
	for _, info := range committed {
		// Skip task checkpoints in the main listing
		if info.IsTask {
			continue
		}

		// Filter: only include checkpoints that are linked to commits on this branch
		if _, ok := checkpointMessages[info.CheckpointID]; !ok {
			continue
		}

		points = append(points, strategy.RewindPoint{
			ID:           info.CheckpointID,
			CheckpointID: info.CheckpointID,
			SessionID:    info.SessionID,
			Date:         info.CreatedAt,
			Message:      "Checkpoint " + info.CheckpointID[:8],
			IsLogsOnly:   true, // Committed checkpoints are logs-only (can't full rewind)
		})
	}

	return points, checkpointMessages
}

// getBranchCheckpointInfo returns checkpoint IDs and their associated commit messages
// for commits on the current branch (by looking at Entire-Checkpoint trailers).
// Returns: map of checkpoint ID -> commit message (subject line)
func getBranchCheckpointInfo(repo *git.Repository) map[string]string {
	checkpointMessages := make(map[string]string)

	head, err := repo.Head()
	if err != nil {
		return checkpointMessages
	}

	// Find the merge-base with main/master to only include commits unique to this branch
	mainRef := strategy.GetMainBranchHash(repo)

	// If on main branch or can't find main, include all checkpoints from recent commits
	if mainRef == plumbing.ZeroHash {
		iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
		if err != nil {
			return checkpointMessages
		}
		const maxCommits = 100
		count := 0
		_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Sentinel error stops iteration
			if count >= maxCommits {
				return errors.New("limit reached")
			}
			count++
			extractCheckpointInfo(c, checkpointMessages)
			return nil
		})
		return checkpointMessages
	}

	// Find merge-base between HEAD and main using go-git's native method
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return checkpointMessages
	}

	mainCommit, err := repo.CommitObject(mainRef)
	if err != nil {
		return checkpointMessages
	}

	mergeBaseCommits, err := headCommit.MergeBase(mainCommit)
	if err != nil || len(mergeBaseCommits) == 0 {
		return checkpointMessages
	}
	mergeBase := mergeBaseCommits[0].Hash

	// Walk from HEAD back to merge-base, collecting checkpoint IDs from trailers
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return checkpointMessages
	}

	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Sentinel error stops iteration
		// Stop when we reach the merge-base (commits from main)
		if c.Hash == mergeBase {
			return errors.New("reached merge-base")
		}
		extractCheckpointInfo(c, checkpointMessages)
		return nil
	})

	return checkpointMessages
}

// extractCheckpointInfo extracts the Entire-Checkpoint trailer and commit subject
// and adds them to the checkpointMessages map.
func extractCheckpointInfo(c *object.Commit, checkpointMessages map[string]string) {
	const trailerPrefix = "Entire-Checkpoint: "
	for _, line := range strings.Split(c.Message, "\n") {
		if checkpointID, found := strings.CutPrefix(line, trailerPrefix); found {
			checkpointID = strings.TrimSpace(checkpointID)
			if checkpointID != "" {
				// Get the commit subject (first line of message)
				subject := strings.Split(c.Message, "\n")[0]
				// If multiple commits share a checkpoint, concatenate messages
				if existing, ok := checkpointMessages[checkpointID]; ok {
					checkpointMessages[checkpointID] = existing + "; " + subject
				} else {
					checkpointMessages[checkpointID] = subject
				}
			}
			break // Only one checkpoint trailer per commit
		}
	}
}

// formatBranchExplain formats the branch-level explain output.
// Parameters:
//   - branchName: the current branch name
//   - checkpoints: the checkpoints with metadata to display (already limited)
//   - branchSummary: optional AI-generated summary for the entire branch
//   - verbose: show additional details like session IDs
//   - full: show complete transcript (future use)
//   - isDefault: whether on main/master branch
//   - limit: the applied limit (0 means no limit)
//   - totalCount: total number of checkpoints before limiting
func formatBranchExplain(branchName string, checkpoints []checkpointWithMeta, branchSummary *Summary, verbose, full bool, isDefault bool, limit, totalCount int) string {
	var sb strings.Builder

	// Header
	fmt.Fprintf(&sb, "Branch: %s\n", branchName)
	if isDefault && limit > 0 && totalCount > limit {
		fmt.Fprintf(&sb, "Checkpoints: %d (showing last %d)\n", totalCount, limit)
	} else {
		fmt.Fprintf(&sb, "Checkpoints: %d\n", len(checkpoints))
	}

	sb.WriteString("\n")

	// Branch-level intent/outcome - use AI summary if available
	if branchSummary != nil && branchSummary.Intent != "" {
		fmt.Fprintf(&sb, "Intent:\n  %s\n", branchSummary.Intent)
	} else {
		sb.WriteString("Intent:\n  (run with --generate to create summary)\n")
	}

	if branchSummary != nil && branchSummary.Outcome != "" {
		fmt.Fprintf(&sb, "Outcome:\n  %s\n", branchSummary.Outcome)
	} else {
		sb.WriteString("Outcome:\n  (run with --generate to create summary)\n")
	}

	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("\u2500", 40)) // Unicode box drawing character for line
	sb.WriteString("\n\n")

	// Checkpoint details
	for i, cp := range checkpoints {
		point := cp.Point
		meta := cp.Metadata

		// Build intent/outcome with markers
		intent, intentMarker := getIntentWithMarker(cp)
		outcome, outcomeMarker := getOutcomeWithMarker(cp)

		if verbose {
			// Verbose mode: use detailed format (same as --checkpoint)
			if i > 0 {
				sb.WriteString("\n────────────────────────────────────────\n\n")
			}

			shortID := point.CheckpointID
			if len(shortID) > 12 {
				shortID = shortID[:12]
			}

			var filesTouched []string
			if meta != nil {
				filesTouched = meta.FilesTouched
			}

			formatCheckpointDetail(&sb, checkpointFormatData{
				ShortID:       shortID,
				FullID:        point.CheckpointID,
				SessionID:     point.SessionID,
				Created:       point.Date,
				FilesTouched:  filesTouched,
				Transcript:    cp.Transcript,
				Intent:        intent,
				Outcome:       outcome,
				IntentMarker:  intentMarker,
				OutcomeMarker: outcomeMarker,
			})

			// In full mode, show the complete transcript
			if full && cp.Transcript != "" {
				sb.WriteString("\n--- Full Transcript ---\n")
				sb.WriteString(cp.Transcript)
				sb.WriteString("\n--- End Transcript ---\n")
			}
		} else {
			// Compact mode: simple list format
			id := point.CheckpointID
			if len(id) > 12 {
				id = id[:12]
			}

			fmt.Fprintf(&sb, "[%s] %s\n", id, point.Date.Format("2006-01-02 15:04"))
			fmt.Fprintf(&sb, "  Intent:%s %s\n", intentMarker, intent)
			fmt.Fprintf(&sb, "  Outcome:%s %s\n", outcomeMarker, outcome)

			// In full mode, show the complete transcript even in compact mode
			if full && cp.Transcript != "" {
				sb.WriteString("\n  --- Transcript ---\n")
				for _, line := range strings.Split(cp.Transcript, "\n") {
					if line != "" {
						sb.WriteString("  ")
						sb.WriteString(line)
						sb.WriteString("\n")
					}
				}
				sb.WriteString("  --- End Transcript ---\n")
			}

			sb.WriteString("\n")
		}
	}

	// Footer for limited view
	if isDefault && limit > 0 && totalCount > limit {
		sb.WriteString(fmt.Sprintf("(%d total checkpoints. Use --limit N to adjust)\n", totalCount))
	}

	return sb.String()
}

// getIntentWithMarker extracts the intent and marker from a checkpoint.
// Markers: "*" = heuristic, "^" = commit message fallback.
func getIntentWithMarker(cp checkpointWithMeta) (intent, marker string) {
	intent = notGeneratedPlaceholder
	switch {
	case cp.Metadata != nil && cp.Metadata.Intent != "":
		intent = cp.Metadata.Intent
		switch cp.Metadata.SummarySource {
		case checkpoint.SummarySourceHeuristic:
			marker = "*"
		case checkpoint.SummarySourceCommit:
			marker = "^"
		}
	case cp.GeneratedSummary != nil && cp.GeneratedSummary.Intent != "":
		intent = cp.GeneratedSummary.Intent
		if cp.UsedHeuristic {
			marker = "*"
		}
	case cp.CommitMessage != "":
		intent = cp.CommitMessage
		if len(intent) > 80 {
			intent = intent[:77] + "..."
		}
		marker = "^"
	}
	return
}

// getOutcomeWithMarker extracts the outcome and marker from a checkpoint.
func getOutcomeWithMarker(cp checkpointWithMeta) (outcome, marker string) {
	outcome = notGeneratedPlaceholder
	if cp.Metadata != nil && cp.Metadata.Outcome != "" {
		outcome = cp.Metadata.Outcome
		if cp.Metadata.SummarySource == checkpoint.SummarySourceHeuristic {
			marker = "*"
		}
	} else if cp.GeneratedSummary != nil && cp.GeneratedSummary.Outcome != "" {
		outcome = cp.GeneratedSummary.Outcome
		if cp.UsedHeuristic {
			marker = "*"
		}
	}
	return
}

// checkpointFormatData holds the data needed to format a checkpoint's details.
type checkpointFormatData struct {
	ShortID       string
	FullID        string
	SessionID     string
	Created       time.Time
	FilesTouched  []string
	Transcript    string
	Intent        string
	Outcome       string
	IntentMarker  string // "*" for heuristic, "^" for commit message
	OutcomeMarker string
}

// formatCheckpointDetail formats a single checkpoint's details for verbose output.
func formatCheckpointDetail(sb *strings.Builder, data checkpointFormatData) {
	// Header: Checkpoint ID and timestamp
	fmt.Fprintf(sb, "Checkpoint: %s\n", data.ShortID)
	if !data.Created.IsZero() {
		fmt.Fprintf(sb, "Created: %s\n", data.Created.Format("2006-01-02 15:04:05"))
	}
	sb.WriteString("\n")

	// Intent and Outcome
	if data.Intent != "" && data.Intent != notGeneratedPlaceholder {
		fmt.Fprintf(sb, "Intent:%s %s\n", data.IntentMarker, data.Intent)
	} else {
		sb.WriteString("Intent: " + notGeneratedPlaceholder + "\n")
	}
	if data.Outcome != "" && data.Outcome != notGeneratedPlaceholder {
		fmt.Fprintf(sb, "Outcome:%s %s\n", data.OutcomeMarker, data.Outcome)
	} else {
		sb.WriteString("Outcome: " + notGeneratedPlaceholder + "\n")
	}
	sb.WriteString("\n")

	// Metadata
	if data.SessionID != "" {
		fmt.Fprintf(sb, "Session ID: %s\n", data.SessionID)
	}
	fmt.Fprintf(sb, "Transcript size: %d bytes\n", len(data.Transcript))
	fmt.Fprintf(sb, "Files touched: %d\n", len(data.FilesTouched))

	// Parse transcript and show message counts
	if data.Transcript != "" {
		if transcript, err := parseTranscriptFromBytes([]byte(data.Transcript)); err == nil {
			sb.WriteString("\n")
			fmt.Fprintf(sb, "Transcript messages: %d\n", len(transcript))
			humanCount, toolResultCount, assistantCount, otherCount := countMessageTypes(transcript)
			fmt.Fprintf(sb, "  Human prompts: %d\n", humanCount)
			fmt.Fprintf(sb, "  Tool results: %d\n", toolResultCount)
			fmt.Fprintf(sb, "  Assistant: %d\n", assistantCount)
			fmt.Fprintf(sb, "  Other: %d\n", otherCount)

			// Show formatted size and preview
			formatted := formatTranscriptForAI(transcript)
			fmt.Fprintf(sb, "\nFormatted transcript size: %d bytes\n", len(formatted))

			preview := formatted
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			fmt.Fprintf(sb, "\n--- Formatted Transcript Preview ---\n%s\n--- End Preview ---\n", preview)
		}
	}
}

// countMessageTypes counts human prompts, tool results, assistant messages, and other types.
func countMessageTypes(transcript []transcriptLine) (human, toolResult, assistant, other int) {
	for _, line := range transcript {
		switch line.Type {
		case transcriptTypeUser:
			if isToolResultMessage(line.Message) {
				toolResult++
			} else {
				human++
			}
		case transcriptTypeAssistant:
			assistant++
		default:
			other++
		}
	}
	return
}

// isToolResultMessage checks if a user message contains only tool_result content.
func isToolResultMessage(message json.RawMessage) bool {
	var msg struct {
		Content interface{} `json:"content"`
	}
	if err := json.Unmarshal(message, &msg); err != nil {
		return false
	}

	// String content is human input
	if _, ok := msg.Content.(string); ok {
		return false
	}

	// Array content - check if all items are tool_result
	if arr, ok := msg.Content.([]interface{}); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] != contentTypeToolResult {
					return false
				}
			}
		}
		return len(arr) > 0
	}

	return false
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

// loadCheckpointMetadata loads the CommittedMetadata for a checkpoint ID.
// Returns nil if metadata cannot be loaded (e.g., no repository, no sessions branch,
// checkpoint doesn't exist, or any other error).
func loadCheckpointMetadata(checkpointID string) *checkpoint.CommittedMetadata {
	repo, err := openRepository()
	if err != nil {
		return nil
	}

	store := checkpoint.NewGitStore(repo)
	result, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil || result == nil {
		return nil
	}

	return &result.Metadata
}

// batchSaveSummariesWithBranch persists checkpoint summaries and branch summary in a single commit.
// Errors are silently ignored since saving is best-effort (display still works).
func batchSaveSummariesWithBranch(updates []checkpoint.UpdateSummaryOptions, branchSummary *checkpoint.BranchSummary) {
	repo, err := openRepository()
	if err != nil {
		return
	}

	store := checkpoint.NewGitStore(repo)
	//nolint:errcheck,gosec // Best-effort save - display works even if save fails
	store.BatchUpdateWithBranchSummary(context.Background(), updates, branchSummary)
}

// getCurrentHeadShort returns the short SHA of the current HEAD commit.
// Returns empty string if HEAD cannot be resolved.
func getCurrentHeadShort() string {
	repo, err := openRepository()
	if err != nil {
		return ""
	}

	head, err := repo.Head()
	if err != nil {
		return ""
	}

	sha := head.Hash().String()
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// extractCheckpointIDs extracts checkpoint IDs from a slice of checkpointWithMeta.
func extractCheckpointIDs(checkpoints []checkpointWithMeta) []string {
	ids := make([]string, 0, len(checkpoints))
	for _, cp := range checkpoints {
		id := cp.Point.CheckpointID
		if len(id) > 12 {
			id = id[:12]
		}
		ids = append(ids, id)
	}
	return ids
}

// loadCheckpointTranscript loads the raw transcript content for a checkpoint.
// Returns empty string if the transcript cannot be loaded.
func loadCheckpointTranscript(checkpointID string) string {
	repo, err := openRepository()
	if err != nil {
		return ""
	}

	store := checkpoint.NewGitStore(repo)
	result, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil || result == nil || len(result.Transcript) == 0 {
		return ""
	}

	return string(result.Transcript)
}

// generateBranchSummaryFromCheckpointsWithUsage aggregates checkpoint intents into a branch-level summary
// and returns token usage information.
// Returns nil if summarization fails or there are no intents to summarize.
func generateBranchSummaryFromCheckpointsWithUsage(checkpoints []checkpointWithMeta) *SummaryResult {
	// Collect checkpoint intents (prefer stored > generated > commit message)
	var intents []string
	for _, cp := range checkpoints {
		intent := ""
		switch {
		case cp.Metadata != nil && cp.Metadata.Intent != "":
			intent = cp.Metadata.Intent
		case cp.GeneratedSummary != nil && cp.GeneratedSummary.Intent != "":
			intent = cp.GeneratedSummary.Intent
		case cp.CommitMessage != "":
			intent = cp.CommitMessage
		}
		if intent != "" {
			intents = append(intents, intent)
		}
	}

	if len(intents) == 0 {
		return nil
	}

	result, err := GenerateBranchSummaryWithUsage(context.Background(), intents)
	if err != nil {
		// Silent failure - branch summary is optional
		return nil
	}
	return result
}

// generateSummaryForCheckpointWithUsage loads a checkpoint's transcript and generates a summary
// using AI summarization via Claude CLI. Falls back to heuristic extraction if AI fails.
// Returns nil if the transcript cannot be loaded or parsed.
func generateSummaryForCheckpointWithUsage(checkpointID string) *SummaryResult {
	repo, err := openRepository()
	if err != nil {
		return nil
	}

	store := checkpoint.NewGitStore(repo)
	result, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil || result == nil || len(result.Transcript) == 0 {
		return nil
	}

	transcript, err := parseTranscriptFromBytes(result.Transcript)
	if err != nil {
		return nil
	}

	// Use AI summarization which falls back to heuristic extraction if Claude CLI unavailable
	summaryResult, err := GenerateAISummaryWithUsage(context.Background(), transcript)
	if err != nil {
		return nil
	}
	return summaryResult
}

// runExplainCheckpoint shows details for a single checkpoint.
// When generate is true, also generates a summary with verbose debug output.
// This is useful for debugging why certain checkpoints fail AI summarization.
func runExplainCheckpoint(w io.Writer, checkpointIDPrefix string, generate bool) error {
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

	shortID := fullCheckpointID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	// Load checkpoint data
	result, err := store.ReadCommitted(context.Background(), fullCheckpointID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}

	// Get intent and outcome from existing metadata
	intent := notGeneratedPlaceholder
	outcome := notGeneratedPlaceholder
	var intentMarker, outcomeMarker string
	if result.Metadata.Intent != "" {
		intent = result.Metadata.Intent
		if result.Metadata.SummarySource == checkpoint.SummarySourceHeuristic {
			intentMarker = "*"
		}
	}
	if result.Metadata.Outcome != "" {
		outcome = result.Metadata.Outcome
		if result.Metadata.SummarySource == checkpoint.SummarySourceHeuristic {
			outcomeMarker = "*"
		}
	}

	// Use shared formatter for consistent output
	var sb strings.Builder
	formatCheckpointDetail(&sb, checkpointFormatData{
		ShortID:       shortID,
		FullID:        fullCheckpointID,
		SessionID:     result.Metadata.SessionID,
		Created:       result.Metadata.CreatedAt,
		FilesTouched:  result.Metadata.FilesTouched,
		Transcript:    string(result.Transcript),
		Intent:        intent,
		Outcome:       outcome,
		IntentMarker:  intentMarker,
		OutcomeMarker: outcomeMarker,
	})
	fmt.Fprint(w, sb.String())

	// Additional debugging info specific to --checkpoint mode
	fmt.Fprintf(w, "\nFull ID: %s\n", fullCheckpointID)

	// Parse transcript for additional debugging info
	transcript, err := parseTranscriptFromBytes(result.Transcript)
	if err != nil {
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Format transcript for AI and show stats
	formatted := formatTranscriptForAI(transcript)
	fmt.Fprintf(w, "Formatted transcript size: %d bytes\n", len(formatted))

	// Show first 500 chars of formatted transcript
	preview := formatted
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	fmt.Fprintf(w, "\n--- Formatted Transcript Preview ---\n%s\n--- End Preview ---\n", preview)

	// Only generate summary if --generate flag is set
	if !generate {
		fmt.Fprintf(w, "\n(use --generate to create AI summary)\n")
		return nil
	}

	// Generate summary with detailed output
	fmt.Fprintf(w, "\nGenerating AI summary...\n")

	summaryResult, err := GenerateAISummaryWithUsage(context.Background(), transcript)
	if err != nil {
		fmt.Fprintf(w, "Error: %v\n", err)
		return nil
	}

	if summaryResult.UsedFallback {
		fmt.Fprintf(w, "Result: FALLBACK (heuristic)\n")
		fmt.Fprintf(w, "Fallback reason: %s\n", summaryResult.FallbackReason)
	} else {
		fmt.Fprintf(w, "Result: SUCCESS (AI)\n")
		fmt.Fprintf(w, "Input tokens: %d\n", summaryResult.InputTokens)
		fmt.Fprintf(w, "Output tokens: %d\n", summaryResult.OutputTokens)
	}

	fmt.Fprintf(w, "\n--- Summary ---\n")
	if summaryResult.Summary != nil {
		fmt.Fprintf(w, "Intent: %s\n", summaryResult.Summary.Intent)
		fmt.Fprintf(w, "Outcome: %s\n", summaryResult.Summary.Outcome)
		if len(summaryResult.Summary.Learnings) > 0 {
			fmt.Fprintf(w, "Learnings:\n")
			for _, l := range summaryResult.Summary.Learnings {
				fmt.Fprintf(w, "  - %s\n", l)
			}
		}
		if len(summaryResult.Summary.FrictionPoints) > 0 {
			fmt.Fprintf(w, "Friction Points:\n")
			for _, f := range summaryResult.Summary.FrictionPoints {
				fmt.Fprintf(w, "  - %s\n", f)
			}
		}

		// Save the summary to the checkpoint
		summarySource := checkpoint.SummarySourceAI
		if summaryResult.UsedFallback {
			summarySource = checkpoint.SummarySourceHeuristic
		}

		authorName, authorEmail := strategy.GetGitAuthorFromRepo(repo)
		updateErr := store.UpdateSummary(context.Background(), checkpoint.UpdateSummaryOptions{
			CheckpointID:   fullCheckpointID,
			Intent:         summaryResult.Summary.Intent,
			Outcome:        summaryResult.Summary.Outcome,
			Learnings:      summaryResult.Summary.Learnings,
			FrictionPoints: summaryResult.Summary.FrictionPoints,
			SummarySource:  summarySource,
			AuthorName:     authorName,
			AuthorEmail:    authorEmail,
		})
		if updateErr != nil {
			fmt.Fprintf(w, "\nWarning: failed to save summary: %v\n", updateErr)
		} else {
			fmt.Fprintf(w, "\nSummary saved to checkpoint.\n")
		}
	}

	return nil
}
