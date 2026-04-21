package strategy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// CommitLogEntry records a single condensed commit in the session's sidecar log.
// Stored as one JSON line per commit in .entire/metadata/<session-id>/commits.jsonl.
type CommitLogEntry struct {
	Hash         string    `json:"hash"`
	ShortHash    string    `json:"short_hash"`
	Subject      string    `json:"subject"`
	Timestamp    time.Time `json:"timestamp"`
	CheckpointID string    `json:"checkpoint_id"`
	SessionID    string    `json:"session_id"`
}

// appendCommitLogEntry appends a single JSON line to the session's commit log.
// Creates the session metadata directory if it does not exist.
func appendCommitLogEntry(ctx context.Context, sessionID string, entry CommitLogEntry) error {
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		return fmt.Errorf("resolving session dir: %w", err)
	}

	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("creating session dir: %w", err)
	}

	logPath := filepath.Join(sessionDirAbs, paths.CommitLogFileName)

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling commit log entry: %w", err)
	}
	data = append(data, '\n')

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // path derived from session ID
	if err != nil {
		return fmt.Errorf("opening commit log: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing commit log entry: %w", err)
	}
	return nil
}

// readCommitLog reads all entries from a session's commit log.
// Returns nil (not an error) if the file does not exist.
func readCommitLog(ctx context.Context, sessionID string) ([]CommitLogEntry, error) {
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		return nil, fmt.Errorf("resolving session dir: %w", err)
	}

	logPath := filepath.Join(sessionDirAbs, paths.CommitLogFileName)

	f, err := os.Open(logPath) //nolint:gosec // path derived from session ID
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening commit log: %w", err)
	}
	defer f.Close()

	var entries []CommitLogEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry CommitLogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue // skip malformed lines
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("scanning commit log: %w", err)
	}
	return entries, nil
}

// buildCommitLog reads the existing commit log from the filesystem and appends
// a new entry, returning the complete log as bytes for inclusion in a checkpoint.
// If the existing file is missing or unreadable, returns just the new entry.
func buildCommitLog(ctx context.Context, sessionID string, newEntry CommitLogEntry) []byte {
	var existing []byte
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	if sessionDirAbs, err := paths.AbsPath(ctx, sessionDir); err == nil {
		logPath := filepath.Join(sessionDirAbs, paths.CommitLogFileName)
		existing, _ = os.ReadFile(logPath) //nolint:errcheck,gosec // best-effort read; missing file is fine
	}

	newLine, err := json.Marshal(newEntry)
	if err != nil {
		return existing
	}
	newLine = append(newLine, '\n')

	return append(existing, newLine...)
}

// commitSubject extracts the first line of a commit message.
func commitSubject(message string) string {
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		return strings.TrimSpace(message[:i])
	}
	return strings.TrimSpace(message)
}
