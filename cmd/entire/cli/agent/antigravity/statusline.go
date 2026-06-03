package antigravity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
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
// otherwise uses <os.UserCacheDir()>/entire/antigravity/status.
func statusDir() (string, error) {
	if override := os.Getenv(statusDirEnv); override != "" {
		return override, nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("antigravity status: resolve user cache dir: %w", err)
	}
	return filepath.Join(cacheDir, "entire", "antigravity", "status"), nil
}

// statusFilePath returns the path for the JSONL snapshot file of a conversation.
// filepath.Base guards against path traversal in the conversation ID.
func statusFilePath(conversationID string) (string, error) {
	dir, err := statusDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(conversationID)+".jsonl"), nil
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

	dir, err := statusDir()
	if err != nil {
		return err
	}
	isNew := false
	filePath := filepath.Join(dir, filepath.Base(p.ConversationID)+".jsonl")

	if _, statErr := os.Stat(filePath); os.IsNotExist(statErr) {
		isNew = true
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("antigravity status: mkdir: %w", err)
	}

	// Dedup: compare compact JSON of the new context_window against the last
	// persisted line's context_window. Read only the last line for performance.
	newCWBytes, err := json.Marshal(p.ContextWindow)
	if err != nil {
		return nil
	}

	if !isNew {
		lastCW, readErr := readLastContextWindow(filePath)
		if readErr == nil && lastCW != nil {
			lastCWBytes, marshalErr := json.Marshal(lastCW)
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
		pruneStaleStatusFiles(dir, p.ConversationID)
	}

	return nil
}

// readLastContextWindow reads only the last non-empty line from a JSONL file
// and returns its context_window for dedup comparison.
func readLastContextWindow(filePath string) (*statusContextWindow, error) {
	//nolint:gosec // filePath is derived from filepath.Base(conversationID)
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("antigravity status: open for dedup: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var lastLine string
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lastLine = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("antigravity status: scan for dedup: %w", err)
	}
	if lastLine == "" {
		return nil, nil //nolint:nilnil // no lines yet — caller handles nil gracefully
	}

	var snap statusSnapshot
	if err := json.Unmarshal([]byte(lastLine), &snap); err != nil {
		return nil, nil //nolint:nilnil // malformed last line — treat as no prior snapshot
	}
	return &snap.ContextWindow, nil
}

// readStatusSnapshots reads all valid snapshot lines from the JSONL file for
// the given conversationID. A missing file returns nil, nil (not an error).
func readStatusSnapshots(conversationID string) ([]statusSnapshot, error) {
	filePath, err := statusFilePath(conversationID)
	if err != nil {
		return nil, err
	}

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
