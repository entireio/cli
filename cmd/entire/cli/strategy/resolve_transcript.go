package strategy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// ResolveTranscriptPath is the exported wrapper around resolveTranscriptPath
// for callers outside the strategy package (e.g. CLI commands streaming a
// live transcript to stdout). The underlying function may update
// state.TranscriptPath when an agent has relocated the file mid-session.
func ResolveTranscriptPath(state *SessionState) (string, error) {
	return resolveTranscriptPath(state)
}

// resolveTranscriptPath returns the current path to the session's transcript file.
// If the file exists at state.TranscriptPath, that path is returned immediately.
//
// If the file is missing (os.ErrNotExist), the function re-resolves the path
// using the agent's ResolveSessionFile method. This handles agents that relocate
// transcripts mid-session (e.g., Cursor CLI switching from a flat layout
// <dir>/<id>.jsonl to a nested layout <dir>/<id>/<id>.jsonl).
//
// On successful re-resolution, state.TranscriptPath is updated so that
// subsequent reads use the correct path without repeating the resolution.
func resolveTranscriptPath(state *SessionState) (string, error) {
	if state.TranscriptPath == "" {
		return "", errors.New("no transcript path in session state")
	}

	// Fast path: file exists at the stored location.
	if _, err := os.Stat(state.TranscriptPath); err == nil {
		return state.TranscriptPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// Non-ENOENT error (permission denied, etc.) — return as-is.
		return "", fmt.Errorf("failed to access transcript: %w", err)
	}

	// File not found — attempt re-resolution via the agent.
	ag, agErr := agent.GetByAgentType(state.AgentType)
	if agErr != nil {
		return "", fmt.Errorf("transcript not found at %s: %w", state.TranscriptPath, os.ErrNotExist)
	}

	// Extract the session dir and agent session ID from the stored path.
	// Stored path format: <sessionDir>/<agentSessionID>.jsonl
	sessionDir := filepath.Dir(state.TranscriptPath)
	base := filepath.Base(state.TranscriptPath)
	agentSessionID := strings.TrimSuffix(base, filepath.Ext(base))

	resolved := ag.ResolveSessionFile(sessionDir, agentSessionID)
	if resolved == state.TranscriptPath {
		// Agent resolved to the same path — file genuinely doesn't exist.
		return "", fmt.Errorf("transcript not found at %s: %w", state.TranscriptPath, os.ErrNotExist)
	}

	// Check if the re-resolved path exists.
	if _, err := os.Stat(resolved); err != nil {
		return "", fmt.Errorf("transcript not found at %s (also tried %s): %w", state.TranscriptPath, resolved, os.ErrNotExist)
	}

	// Update state so subsequent reads use the correct path.
	state.TranscriptPath = resolved
	return resolved, nil
}
