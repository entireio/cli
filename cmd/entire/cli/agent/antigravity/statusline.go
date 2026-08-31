package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// agy's title/statusline hook pipes a state JSON to the configured command on
// every agent state change. The context_window object is the ONLY surface
// where agy exposes token usage — it never appears in transcripts or
// lifecycle hook payloads. AppendStatusSnapshot persists those snapshots so
// the lifecycle can compute per-checkpoint deltas later.
//
// Totals are cumulative per conversation; current_usage is the latest API call.

// statusDirEnv overrides the snapshot cache directory (tests, ops).
const statusDirEnv = "ENTIRE_ANTIGRAVITY_STATUS_DIR"

// statusRetention is how long snapshot files for other conversations are kept.
const statusRetention = 14 * 24 * time.Hour

// statusCurrentUsage mirrors context_window.current_usage in the payload.
type statusCurrentUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// statusContextWindow mirrors context_window in the payload.
type statusContextWindow struct {
	TotalInputTokens  int                 `json:"total_input_tokens"`
	TotalOutputTokens int                 `json:"total_output_tokens"`
	ContextWindowSize int                 `json:"context_window_size,omitempty"`
	CurrentUsage      *statusCurrentUsage `json:"current_usage,omitempty"`
}

// statusSnapshot is one persisted line in <conversation_id>.jsonl.
type statusSnapshot struct {
	Timestamp      string              `json:"ts"`
	ConversationID string              `json:"conversation_id"`
	ContextWindow  statusContextWindow `json:"context_window"`
}

// statuslinePayload is the subset of agy's state JSON we consume.
type statuslinePayload struct {
	ConversationID string               `json:"conversation_id"`
	ContextWindow  *statusContextWindow `json:"context_window"`
}

// statusDir returns the directory used to store snapshot files.
// It honours the ENTIRE_ANTIGRAVITY_STATUS_DIR env override (tests, ops),
// otherwise uses <userdirs.Cache()>/antigravity/status. userdirs is the
// mandated resolver: it honours $XDG_CACHE_HOME on every platform (os.
// UserCacheDir ignores it on darwin, defeating harness isolation) and falls
// back to a throwaway per-process dir under `go test`.
func statusDir() string {
	if override := os.Getenv(statusDirEnv); override != "" {
		return override
	}
	return filepath.Join(userdirs.Cache(), "antigravity", "status")
}

// statusFilePath returns the path for the JSONL snapshot file of a conversation.
// filepath.Base guards against path traversal in the conversation ID.
func statusFilePath(conversationID string) string {
	return filepath.Join(statusDir(), filepath.Base(conversationID)+".jsonl")
}

// AppendStatusSnapshot parses an agy state-JSON payload and appends a snapshot
// to the per-conversation JSONL file. The hot path never returns an error for
// malformed input — only for genuine I/O failures after the file has been opened.
func AppendStatusSnapshot(payload []byte) error {
	var p statuslinePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	if p.ConversationID == "" || p.ContextWindow == nil {
		return nil // missing required fields — silently skip
	}

	filePath := statusFilePath(p.ConversationID)
	isNew := false
	if _, statErr := os.Stat(filePath); os.IsNotExist(statErr) {
		isNew = true
	}

	// The directory necessarily exists once the snapshot file does, so only
	// pay the MkdirAll syscall on the first append of a conversation.
	if isNew {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
			return fmt.Errorf("antigravity status: mkdir: %w", err)
		}
	}

	// Dedup: compare compact JSON of the new context_window against the last
	// persisted line's context_window. readLastStatusSnapshot streams the file
	// keeping only the final line (O(1) memory); the file stays small because
	// this very dedup suppresses unchanged snapshots.
	newCWBytes, err := json.Marshal(p.ContextWindow)
	if err != nil {
		return nil
	}

	if !isNew {
		lastSnap, readErr := readLastStatusSnapshot(filePath)
		if readErr == nil && lastSnap != nil {
			lastCWBytes, marshalErr := json.Marshal(lastSnap.ContextWindow)
			if marshalErr == nil && bytes.Equal(newCWBytes, lastCWBytes) {
				return nil // duplicate — skip
			}
		}
	}

	snap := statusSnapshot{
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
		ConversationID: p.ConversationID,
		ContextWindow:  *p.ContextWindow,
	}
	line, err := json.Marshal(snap)
	if err != nil {
		return nil
	}

	//nolint:gosec // filePath is derived from filepath.Base(conversationID)
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("antigravity status: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("antigravity status: write: %w", err)
	}

	// Best-effort prune of stale files for other conversations when we first
	// create the active file (avoids per-append overhead).
	if isNew {
		pruneStaleStatusFiles(filepath.Dir(filePath), p.ConversationID)
	}

	return nil
}

// SnapshotTokenBaseline returns the latest persisted snapshot for the
// conversation, or nil if none exists yet. A nil baseline is exact only for a
// genuinely fresh conversation; a resumed conversation whose title-tee shim
// hasn't written a snapshot before the first TurnStart will over-count the
// prior cumulative total on that first tracked turn.
func (a *AntigravityAgent) SnapshotTokenBaseline(_ context.Context, sessionID string) (json.RawMessage, error) {
	snap, err := readLastStatusSnapshot(statusFilePath(sessionID))
	if err != nil || snap == nil {
		return nil, nil //nolint:nilerr // ditto (missing file, no lines, malformed)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, nil //nolint:nilerr // ditto
	}
	return raw, nil
}

// CalculateTokenUsageSince computes the delta between the baseline snapshot
// and the latest persisted snapshot.
//
// Exact: InputTokens/OutputTokens (cumulative totals minus baseline totals).
// Best-effort: cache fields and APICallCount, derived from the snapshot lines
// appended after the baseline timestamp (the dedup writer appends ~one line
// per API response, but lines can be missed between agent state changes).
func (a *AntigravityAgent) CalculateTokenUsageSince(_ context.Context, sessionID string, baseline json.RawMessage) (*agent.TokenUsage, error) {
	snaps, err := readStatusSnapshots(sessionID)
	if err != nil || len(snaps) == 0 {
		return nil, nil //nolint:nilerr,nilnil // no data -> no token counts, never an error
	}

	var base statusSnapshot
	if len(baseline) > 0 {
		_ = json.Unmarshal(baseline, &base) //nolint:errcheck // unparseable baseline -> zero baseline
	}

	latest := snaps[len(snaps)-1]
	usage := &agent.TokenUsage{
		InputTokens:  max(0, latest.ContextWindow.TotalInputTokens-base.ContextWindow.TotalInputTokens),
		OutputTokens: max(0, latest.ContextWindow.TotalOutputTokens-base.ContextWindow.TotalOutputTokens),
	}

	// The strictly-after (.After, not >=) filter is load-bearing for
	// multi-turn correctness: turn N+1's baseline IS turn N's latest snapshot,
	// so excluding the equal-timestamp boundary line prevents re-counting it.
	// Changing this to >= would double-count the boundary line every turn.
	baseTS, baseTSErr := time.Parse(time.RFC3339Nano, base.Timestamp)
	for _, s := range snaps {
		// If baseTS is unparseable we count cache/apicalls over all lines; accepted because input/output remain exact via the totals delta.
		if base.Timestamp != "" && baseTSErr == nil {
			ts, parseErr := time.Parse(time.RFC3339Nano, s.Timestamp)
			if parseErr != nil || !ts.After(baseTS) {
				continue
			}
		}
		usage.APICallCount++
		if cu := s.ContextWindow.CurrentUsage; cu != nil {
			usage.CacheCreationTokens += cu.CacheCreationInputTokens
			usage.CacheReadTokens += cu.CacheReadInputTokens
		}
	}

	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheCreationTokens == 0 && usage.CacheReadTokens == 0 {
		return nil, nil //nolint:nilnil // nothing observed this turn
	}
	return usage, nil
}

// statusTailWindow bounds how many bytes readLastStatusSnapshot reads from the
// end of the file. Snapshot lines are well under 1 KB, so 64 KB always covers
// the final line with huge margin.
const statusTailWindow = 64 * 1024

// readLastStatusSnapshot returns the snapshot on the final non-empty line, or
// nil if the file has no usable line. It reads a bounded tail window instead
// of streaming the whole file: it is shared by the per-fire dedup comparison
// in AppendStatusSnapshot and by every-TurnStart SnapshotTokenBaseline, and
// agy fires the title command on each agent state change — a front-to-back
// scan would cost O(file) per fire, O(n^2) over a conversation.
func readLastStatusSnapshot(filePath string) (*statusSnapshot, error) {
	//nolint:gosec // filePath is derived from filepath.Base(conversationID)
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("antigravity status: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("antigravity status: stat: %w", err)
	}

	offset := info.Size() - statusTailWindow
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("antigravity status: read tail: %w", err)
	}

	// When the window starts mid-file, the first chunk may be a partial line —
	// discard through the first newline so only whole lines are considered.
	if offset > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			return nil, nil //nolint:nilnil // single line larger than the window — treat as no usable snapshot
		}
		buf = buf[nl+1:]
	}

	var lastLine []byte
	for _, line := range bytes.Split(buf, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			lastLine = line
		}
	}
	if len(lastLine) == 0 {
		return nil, nil //nolint:nilnil // no lines yet — caller handles nil gracefully
	}

	var snap statusSnapshot
	if err := json.Unmarshal(lastLine, &snap); err != nil {
		return nil, nil //nolint:nilnil // malformed last line — treat as no prior snapshot
	}
	return &snap, nil
}

// readStatusSnapshots reads all valid snapshot lines from the JSONL file for
// the given conversationID. A missing file returns nil, nil (not an error).
func readStatusSnapshots(conversationID string) ([]statusSnapshot, error) {
	filePath := statusFilePath(conversationID)

	//nolint:gosec // filePath is derived from filepath.Base(conversationID)
	f, err := os.Open(filePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("antigravity status: open for read: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var snaps []statusSnapshot
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var snap statusSnapshot
		if err := json.Unmarshal([]byte(line), &snap); err != nil {
			continue // skip malformed lines
		}
		snaps = append(snaps, snap)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("antigravity status: scan: %w", err)
	}
	return snaps, nil
}

// pruneStaleStatusFiles removes JSONL files in dir that are not the active
// conversation and whose mtime is older than statusRetention. Best-effort:
// errors are silently ignored.
func pruneStaleStatusFiles(dir, activeConversationID string) {
	activeFile := filepath.Base(activeConversationID) + ".jsonl"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-statusRetention)
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == activeFile {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
