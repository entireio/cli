package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// fileEditsPath returns the path to the file edits JSONL file for a session.
func (s *StateStore) fileEditsPath(sessionID string) string {
	return filepath.Join(s.stateDir, sessionID+"-edits.jsonl")
}

// AppendFileEdit appends a single FileEdit record to the session's edit log.
// This is an append-only operation -- no read-modify-write cycle.
// Safe for concurrent appenders: uses O_APPEND which is atomic for writes under PIPE_BUF (~4KB).
// FileEdit records are well under this limit with typical file paths.
func (s *StateStore) AppendFileEdit(sessionID string, edit agent.FileEdit) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	if err := os.MkdirAll(s.stateDir, 0o750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	path := s.fileEditsPath(sessionID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: path built from validated session ID
	if err != nil {
		return fmt.Errorf("failed to open file edits log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(edit)
	if err != nil {
		return fmt.Errorf("failed to marshal file edit: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write file edit: %w", err)
	}
	return nil
}

// ReadFileEdits reads all FileEdit records from the session's edit log.
// Returns (nil, nil) if the log doesn't exist.
// Malformed lines are skipped with a warning log (non-fatal).
func (s *StateStore) ReadFileEdits(sessionID string) ([]agent.FileEdit, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("invalid session ID: %w", err)
	}

	path := s.fileEditsPath(sessionID)
	f, err := os.Open(path) //nolint:gosec // G304: path built from validated session ID
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open file edits log: %w", err)
	}
	defer f.Close()

	var edits []agent.FileEdit
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var edit agent.FileEdit
		if err := json.Unmarshal(line, &edit); err != nil {
			log.Printf("[entire] warning: skipped malformed file edit line %d in %s: %v", lineNum, sessionID, err)
			continue
		}
		edits = append(edits, edit)
	}
	if err := scanner.Err(); err != nil {
		return edits, fmt.Errorf("failed to read file edits log: %w", err)
	}
	return edits, nil
}

// ClearFileEdits removes the session's edit log file.
func (s *StateStore) ClearFileEdits(sessionID string) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	path := s.fileEditsPath(sessionID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file edits log: %w", err)
	}
	return nil
}
