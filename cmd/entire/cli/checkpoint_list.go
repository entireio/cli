package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// pendingCheckpointJSON is the machine-readable shape emitted by
// `entire checkpoint list --pending --json` (and the deprecated `rewind --list`
// bridge). Rows that existed before the next-checkpoint preview keep the JSON
// `rewind --list` historically produced byte-for-byte, so downstream consumers
// (integration and e2e test harnesses, external scripts) that parsed
// `rewind --list` keep working after repointing to
// `checkpoint list --pending --json`.
//
// The field set, JSON names, omitempty markers, and the RFC3339 Date encoding
// are load-bearing — this is a stable contract. CondensationID carries the
// checkpoint ID (PendingCheckpoint.CheckpointID) for logs-only points; it is
// empty for uncommitted rows. IsNextCheckpoint and NextCheckpoint appear only
// on next-checkpoint preview rows (omitempty), so the other rows' key sets are
// unchanged. Do not change these without migrating every consumer.
type pendingCheckpointJSON struct {
	ID               string              `json:"id"`
	Message          string              `json:"message"`
	MetadataDir      string              `json:"metadata_dir"`
	Date             string              `json:"date"`
	IsTaskCheckpoint bool                `json:"is_task_checkpoint"`
	ToolUseID        string              `json:"tool_use_id,omitempty"`
	IsLogsOnly       bool                `json:"is_logs_only"`
	CondensationID   string              `json:"condensation_id,omitempty"`
	SessionID        string              `json:"session_id,omitempty"`
	SessionPrompt    string              `json:"session_prompt,omitempty"`
	IsNextCheckpoint bool                `json:"is_next_checkpoint,omitempty"`
	NextCheckpoint   *nextCheckpointJSON `json:"next_checkpoint,omitempty"`
}

// nextCheckpointJSON is the `next_checkpoint` object on a preview row: what
// one session would contribute to the checkpoint the next commit writes. The
// slices are always present (empty, not null) so consumers can range over them.
type nextCheckpointJSON struct {
	Agent        string                         `json:"agent"`
	Turns        int                            `json:"turns"`
	FilesTouched []string                       `json:"files_touched"`
	TaskRecords  []nextCheckpointTaskRecordJSON `json:"task_records"`
	Prompts      []string                       `json:"prompts"`
}

// nextCheckpointTaskRecordJSON is one subagent task record in a preview.
// Status is "running" or "completed".
type nextCheckpointTaskRecordJSON struct {
	ToolUseID    string `json:"tool_use_id"`
	SubagentType string `json:"subagent_type,omitempty"`
	Description  string `json:"description,omitempty"`
	Status       string `json:"status"`
}

// pendingCheckpointsLimit caps how many pending checkpoints (both shapes) the
// pending views request. Matches the historical `rewind --list` cap of 20 so
// the migrated output is identical.
const pendingCheckpointsLimit = 20

// pendingListing is the pending dataset: the next-checkpoint preview for the
// sessions in this worktree, then the existing rows (subagent task records of
// sessions on HEAD, logs-only resume points).
type pendingListing struct {
	previews []strategy.NextCheckpointPreview
	points   []strategy.PendingCheckpoint
}

// loadPendingListing gathers the pending dataset. A task-record row whose
// session already has a preview is dropped: the preview lists that session's
// task records, so the row would only repeat it.
func loadPendingListing(ctx context.Context) (pendingListing, error) {
	strat := GetStrategy(ctx)

	previews, err := strat.PreviewNextCheckpoint(ctx)
	if err != nil {
		return pendingListing{}, fmt.Errorf("failed to preview the next checkpoint: %w", err)
	}
	points, err := strat.ListPendingCheckpoints(ctx, pendingCheckpointsLimit)
	if err != nil {
		return pendingListing{}, fmt.Errorf("failed to list pending checkpoints: %w", err)
	}

	previewed := make(map[string]bool, len(previews))
	for _, p := range previews {
		previewed[p.SessionID] = true
	}
	kept := make([]strategy.PendingCheckpoint, 0, len(points))
	for _, p := range points {
		if p.IsTaskCheckpoint && previewed[p.SessionID] {
			continue
		}
		kept = append(kept, p)
	}
	return pendingListing{previews: previews, points: kept}, nil
}

// runCheckpointPendingListJSON emits the pending dataset as a JSON array:
// next-checkpoint preview rows first, then the task-record and logs-only rows.
// This is also the implementation behind the deprecated `rewind --list` bridge.
func runCheckpointPendingListJSON(ctx context.Context, w io.Writer) error {
	listing, err := loadPendingListing(ctx)
	if err != nil {
		return err
	}

	output := make([]pendingCheckpointJSON, 0, len(listing.previews)+len(listing.points))
	for _, p := range listing.previews {
		output = append(output, nextCheckpointRowJSON(p))
	}
	for _, p := range listing.points {
		output = append(output, pendingCheckpointJSON{
			ID:               p.ID,
			Message:          p.Message,
			MetadataDir:      p.MetadataDir,
			Date:             p.Date.Format(time.RFC3339),
			IsTaskCheckpoint: p.IsTaskCheckpoint,
			ToolUseID:        p.ToolUseID,
			IsLogsOnly:       p.IsLogsOnly,
			CondensationID:   p.CheckpointID.String(),
			SessionID:        p.SessionID,
			SessionPrompt:    p.SessionPrompt,
		})
	}

	data, err := jsonutil.MarshalIndentWithNewline(output, "", "  ")
	if err != nil {
		return err //nolint:wrapcheck // parity with the former rewind --list path
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// nextCheckpointRowJSON renders a preview as a pending row. It has no ID: the
// checkpoint does not exist until a commit condenses it.
func nextCheckpointRowJSON(p strategy.NextCheckpointPreview) pendingCheckpointJSON {
	records := make([]nextCheckpointTaskRecordJSON, 0, len(p.TaskRecords))
	for _, rec := range p.TaskRecords {
		status := "running"
		if rec.Completed {
			status = "completed"
		}
		records = append(records, nextCheckpointTaskRecordJSON{
			ToolUseID:    rec.ToolUseID,
			SubagentType: rec.SubagentType,
			Description:  rec.Description,
			Status:       status,
		})
	}
	files := p.FilesTouched
	if files == nil {
		files = []string{}
	}
	prompts := p.Prompts
	if prompts == nil {
		prompts = []string{}
	}
	return pendingCheckpointJSON{
		Message:          nextCheckpointSummary(p),
		MetadataDir:      paths.SessionMetadataDirFromSessionID(p.SessionID),
		Date:             p.LastActivity.Format(time.RFC3339),
		SessionID:        p.SessionID,
		SessionPrompt:    p.LastPrompt,
		IsNextCheckpoint: true,
		NextCheckpoint: &nextCheckpointJSON{
			Agent:        string(p.Agent),
			Turns:        p.Turns,
			FilesTouched: files,
			TaskRecords:  records,
			Prompts:      prompts,
		},
	}
}

// nextCheckpointSummary is the one-line description of a preview, e.g.
// "Next checkpoint: 2 turns, 3 files, 1 task".
func nextCheckpointSummary(p strategy.NextCheckpointPreview) string {
	parts := []string{
		pluralCount(p.Turns, "turn", "turns"),
		pluralCount(len(p.FilesTouched), "file", "files"),
	}
	if len(p.TaskRecords) > 0 {
		parts = append(parts, pluralCount(len(p.TaskRecords), "task", "tasks"))
	}
	return "Next checkpoint: " + strings.Join(parts, ", ")
}

func pluralCount(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// runCheckpointPendingListHuman prints the pending dataset: a preview of the
// next checkpoint per session in this worktree, then the task-record and
// logs-only rows, each with the label format the former interactive rewind
// picker used (see pendingCheckpointLabel).
func runCheckpointPendingListHuman(ctx context.Context, w io.Writer) error {
	listing, err := loadPendingListing(ctx)
	if err != nil {
		return err
	}

	if len(listing.previews) == 0 && len(listing.points) == 0 {
		fmt.Fprintln(w, "No pending checkpoints found.")
		fmt.Fprintln(w, "Agent work is recorded in session state at the end of each turn and becomes a checkpoint when you commit it.")
		return nil
	}

	if len(listing.previews) > 0 {
		fmt.Fprintln(w, "Next checkpoint (written when you commit):")
		for _, p := range listing.previews {
			renderNextCheckpointPreview(w, p)
		}
		if len(listing.points) > 0 {
			fmt.Fprintln(w)
		}
	}

	multi := hasMultipleSessions(listing.points)
	for _, p := range listing.points {
		fmt.Fprintln(w, pendingCheckpointLabel(p, multi))
	}
	return nil
}

// previewPromptWidth caps each prompt in the human preview; the JSON carries
// them in full.
const previewPromptWidth = 80

// renderNextCheckpointPreview prints one session's preview block.
func renderNextCheckpointPreview(w io.Writer, p strategy.NextCheckpointPreview) {
	header := "  Session " + tuiutil.SanitizeTerminalLabel(p.SessionID)
	if p.Agent != "" {
		header += " (" + tuiutil.SanitizeTerminalLabel(string(p.Agent)) + ")"
	}
	fmt.Fprintf(w, "%s: %s\n", header, strings.TrimPrefix(nextCheckpointSummary(p), "Next checkpoint: "))
	for _, f := range p.FilesTouched {
		fmt.Fprintf(w, "    file    %s\n", tuiutil.SanitizeTerminalLabel(f))
	}
	for _, rec := range p.TaskRecords {
		shortID := rec.ToolUseID
		if len(shortID) > id.ShortIDLength {
			shortID = shortID[:id.ShortIDLength]
		}
		message := strategy.FormatSubagentRunningMessage(rec.SubagentType, rec.Description, shortID)
		if rec.Completed {
			message = strategy.FormatSubagentEndMessage(rec.SubagentType, rec.Description, shortID)
		}
		fmt.Fprintf(w, "    task    %s\n", tuiutil.SanitizeTerminalLabel(message))
	}
	for _, prompt := range p.Prompts {
		label := stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), previewPromptWidth, "...")
		fmt.Fprintf(w, "    prompt  %s\n", tuiutil.SanitizeTerminalLabel(label))
	}
}

// hasMultipleSessions reports whether the points span more than one session,
// which controls whether per-line session identifiers are shown.
func hasMultipleSessions(points []strategy.PendingCheckpoint) bool {
	sessionIDs := make(map[string]bool)
	for _, p := range points {
		if p.SessionID != "" {
			sessionIDs[p.SessionID] = true
		}
	}
	return len(sessionIDs) > 1
}

// pendingCheckpointLabel renders a single pending checkpoint as a display label for the
// `checkpoint list --pending` human view. When hasMultipleSessions is true, a
// sanitized session prompt is appended to help disambiguate concurrent
// sessions.
func pendingCheckpointLabel(p strategy.PendingCheckpoint, hasMultipleSessions bool) string {
	timestamp := p.Date.Format("2006-01-02 15:04")

	sessionLabel := ""
	if hasMultipleSessions && p.SessionPrompt != "" {
		sessionLabel = fmt.Sprintf(" [%s]", tuiutil.SanitizeTerminalLabel(p.SessionPrompt))
	}

	switch {
	case p.IsLogsOnly:
		// Committed checkpoint - show commit sha (this is the real user commit)
		shortID := p.ID
		if len(shortID) >= 7 {
			shortID = shortID[:7]
		}
		return fmt.Sprintf("%s (%s) %s%s", shortID, timestamp, tuiutil.SanitizeTerminalLabel(p.Message), sessionLabel)
	case p.IsTaskCheckpoint:
		// Task record (uncommitted) - no ID until a commit condenses it
		return fmt.Sprintf("        (%s) [Task] %s%s", timestamp, tuiutil.SanitizeTerminalLabel(p.Message), sessionLabel)
	default:
		return fmt.Sprintf("        (%s) %s%s", timestamp, tuiutil.SanitizeTerminalLabel(p.Message), sessionLabel)
	}
}
