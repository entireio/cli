package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// pendingRewindPointJSON is the machine-readable shape emitted by
// `entire checkpoint list --pending --json` (and the deprecated `rewind --list`
// bridge). It is byte-for-byte the JSON that `rewind --list` historically
// produced, so downstream consumers (integration and e2e test harnesses,
// external scripts) that parsed `rewind --list` keep working unchanged after
// repointing to `checkpoint list --pending --json`.
//
// The field set, JSON names, omitempty markers, and the RFC3339 Date encoding
// are load-bearing — this is a stable contract. CondensationID carries the
// checkpoint ID (RewindPoint.CheckpointID) for logs-only points; it is empty
// for shadow-branch (uncommitted) points. Do not change these without
// migrating every consumer.
type pendingRewindPointJSON struct {
	ID               string `json:"id"`
	Message          string `json:"message"`
	MetadataDir      string `json:"metadata_dir"`
	Date             string `json:"date"`
	IsTaskCheckpoint bool   `json:"is_task_checkpoint"`
	ToolUseID        string `json:"tool_use_id,omitempty"`
	IsLogsOnly       bool   `json:"is_logs_only"`
	CondensationID   string `json:"condensation_id,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	SessionPrompt    string `json:"session_prompt,omitempty"`
}

// pendingRewindPointsLimit caps how many live shadow-branch rewind points the
// pending views request. Matches the historical `rewind --list` cap of 20 so
// the migrated output is identical.
const pendingRewindPointsLimit = 20

// runCheckpointPendingListJSON emits the live shadow-branch rewind points as
// JSON. This is the drop-in replacement for (and the implementation behind)
// the deprecated `rewind --list` bridge: same dataset (strategy.GetRewindPoints),
// same cap, same JSON shape.
func runCheckpointPendingListJSON(ctx context.Context, w io.Writer) error {
	start := GetStrategy(ctx)

	points, err := start.GetRewindPoints(ctx, pendingRewindPointsLimit)
	if err != nil {
		return fmt.Errorf("failed to find rewind points: %w", err)
	}

	output := make([]pendingRewindPointJSON, len(points))
	for i, p := range points {
		output[i] = pendingRewindPointJSON{
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
		}
	}

	data, err := jsonutil.MarshalIndentWithNewline(output, "", "  ")
	if err != nil {
		return err //nolint:wrapcheck // parity with the former rewind --list path
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// runCheckpointPendingListHuman prints the live shadow-branch rewind points in
// a human-readable list. `rewind --list` was JSON-only, so there is no legacy
// human output to mirror; this renders each point with the same label format
// the former interactive rewind picker used (see rewindPointLabel).
func runCheckpointPendingListHuman(ctx context.Context, w io.Writer) error {
	start := GetStrategy(ctx)

	points, err := start.GetRewindPoints(ctx, pendingRewindPointsLimit)
	if err != nil {
		return fmt.Errorf("failed to find rewind points: %w", err)
	}

	if len(points) == 0 {
		fmt.Fprintln(w, "No pending rewind points found.")
		fmt.Fprintln(w, "Pending rewind points are created automatically during active agent sessions.")
		return nil
	}

	multi := hasMultipleSessions(points)
	for _, p := range points {
		fmt.Fprintln(w, rewindPointLabel(p, multi))
	}
	return nil
}

// hasMultipleSessions reports whether the points span more than one session,
// which controls whether per-line session identifiers are shown.
func hasMultipleSessions(points []strategy.RewindPoint) bool {
	sessionIDs := make(map[string]bool)
	for _, p := range points {
		if p.SessionID != "" {
			sessionIDs[p.SessionID] = true
		}
	}
	return len(sessionIDs) > 1
}

// rewindPointLabel renders a single rewind point as a display label for the
// `checkpoint list --pending` human view. When hasMultipleSessions is true, a
// sanitized session prompt is appended to help disambiguate concurrent
// sessions.
func rewindPointLabel(p strategy.RewindPoint, hasMultipleSessions bool) string {
	timestamp := p.Date.Format("2006-01-02 15:04")

	sessionLabel := ""
	if hasMultipleSessions && p.SessionPrompt != "" {
		sessionLabel = fmt.Sprintf(" [%s]", sanitizeForTerminal(p.SessionPrompt))
	}

	switch {
	case p.IsLogsOnly:
		// Committed checkpoint - show commit sha (this is the real user commit)
		shortID := p.ID
		if len(shortID) >= 7 {
			shortID = shortID[:7]
		}
		return fmt.Sprintf("%s (%s) %s%s", shortID, timestamp, sanitizeForTerminal(p.Message), sessionLabel)
	case p.IsTaskCheckpoint:
		// Task checkpoint (uncommitted) - no sha shown
		return fmt.Sprintf("        (%s) [Task] %s%s", timestamp, sanitizeForTerminal(p.Message), sessionLabel)
	default:
		// Shadow checkpoint (uncommitted) - no sha shown (internal commit)
		return fmt.Sprintf("        (%s) %s%s", timestamp, sanitizeForTerminal(p.Message), sessionLabel)
	}
}

// sanitizeForTerminal removes or replaces characters that cause rendering issues
// in terminal UI components. This includes emojis with skin-tone modifiers and
// other multi-codepoint characters that confuse width calculations.
func sanitizeForTerminal(s string) string {
	var result strings.Builder
	result.Grow(len(s))

	for _, r := range s {
		// Skip emoji skin tone modifiers (U+1F3FB to U+1F3FF)
		if r >= 0x1F3FB && r <= 0x1F3FF {
			continue
		}
		// Skip zero-width joiners used in emoji sequences
		if r == 0x200D {
			continue
		}
		// Skip variation selectors (U+FE00 to U+FE0F)
		if r >= 0xFE00 && r <= 0xFE0F {
			continue
		}
		// Keep printable characters and common whitespace
		if unicode.IsPrint(r) || r == '\t' || r == '\n' {
			result.WriteRune(r)
		}
	}

	return result.String()
}
