package strategy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"entire.io/cli/cmd/entire/cli/paths"
)

// RestoreSessionResult captures the outcome of a session restore operation.
type RestoreSessionResult struct {
	Status  SessionRestoreStatus
	Written bool   // Whether the file was actually written
	Path    string // The destination path
}

// RestoreSessionFile restores a session transcript to the filesystem with safety checks.
// This is the single entry point for all session restoration operations.
//
// Parameters:
//   - destPath: Where to write the session file
//   - transcript: The session transcript bytes to write
//   - force: If true, overwrite even if local file has newer timestamps
//
// Returns:
//   - RestoreSessionResult with status and whether file was written
//   - Error if restore failed (not for conflicts - those return StatusLocalNewer)
//
// Behavior:
//   - If file doesn't exist: writes file, returns StatusNew
//   - If file exists and unchanged: skips write, returns StatusUnchanged
//   - If checkpoint is newer: writes file, returns StatusCheckpointNewer
//   - If local is newer and force=false: skips write, returns StatusLocalNewer
//   - If local is newer and force=true: writes file, returns StatusLocalNewer
func RestoreSessionFile(destPath string, transcript []byte, force bool) (*RestoreSessionResult, error) {
	if len(transcript) == 0 {
		return nil, errors.New("transcript is empty")
	}

	result := &RestoreSessionResult{
		Path: destPath,
	}

	// Check timestamp status
	localTime := paths.GetLastTimestampFromFile(destPath)
	checkpointTime := paths.GetLastTimestampFromBytes(transcript)
	result.Status = ClassifyTimestamps(localTime, checkpointTime)

	// Determine whether to proceed with write
	shouldWrite := false
	switch result.Status {
	case StatusNew:
		// File doesn't exist, always write
		shouldWrite = true
	case StatusUnchanged:
		// Files are the same, skip
		shouldWrite = false
	case StatusCheckpointNewer:
		// Checkpoint has newer content, update
		shouldWrite = true
	case StatusLocalNewer:
		// Local is newer - only write if forced
		shouldWrite = force
	}

	if !shouldWrite {
		return result, nil
	}

	// Ensure directory exists
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Write the file
	if err := os.WriteFile(destPath, transcript, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write session file: %w", err)
	}

	result.Written = true
	return result, nil
}

// RestoreSessionWithPrompt restores a session file, prompting for confirmation if
// the local file has newer timestamps.
//
// This is a convenience wrapper around RestoreSessionFile for interactive use.
// For batch operations (like multi-session restore), use RestoreSessionFile directly
// and handle prompting at the batch level.
func RestoreSessionWithPrompt(destPath string, transcript []byte, sessionInfo SessionRestoreInfo) (*RestoreSessionResult, error) {
	// First check the status without writing
	localTime := paths.GetLastTimestampFromFile(destPath)
	checkpointTime := paths.GetLastTimestampFromBytes(transcript)
	status := ClassifyTimestamps(localTime, checkpointTime)

	// If local is newer, prompt for confirmation
	if status == StatusLocalNewer {
		shouldOverwrite, err := PromptOverwriteNewerLogs([]SessionRestoreInfo{sessionInfo})
		if err != nil {
			return nil, err
		}
		if !shouldOverwrite {
			return &RestoreSessionResult{
				Path:    destPath,
				Status:  StatusLocalNewer,
				Written: false,
			}, nil
		}
		// User confirmed, proceed with force
		return RestoreSessionFile(destPath, transcript, true)
	}

	// No conflict, proceed normally
	return RestoreSessionFile(destPath, transcript, false)
}
