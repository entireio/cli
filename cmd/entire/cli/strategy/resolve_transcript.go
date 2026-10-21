package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// ResolveTranscriptPath is the exported wrapper around resolveTranscriptPath
// for callers outside the strategy package (e.g. CLI commands streaming a
// live transcript to stdout). The underlying function may update
// state.TranscriptPath when an agent has relocated the file mid-session.
func ResolveTranscriptPath(ctx context.Context, state *SessionState) (string, error) {
	return resolveTranscriptPath(ctx, state)
}

// resolveTranscriptPath returns the current path to the session's transcript file.
// If the file exists at state.TranscriptPath, that path is returned immediately.
//
// If the file is missing (os.ErrNotExist), the function re-resolves the path
// using the agent's ResolveSessionFile method. This handles agents that relocate
// transcripts mid-session (e.g., Cursor CLI switching from a flat layout
// <dir>/<id>.jsonl to a nested layout <dir>/<id>/<id>.jsonl).
//
// Failing that, it looks in the agent's session dir for each worktree of the
// repository, the one this process runs in first (movedTranscriptPath): an
// agent whose session dir is keyed by its working directory (Claude Code)
// writes there once it moves into another worktree, e.g. with EnterWorktree.
//
// On successful re-resolution, state.TranscriptPath is updated so that
// subsequent reads use the correct path without repeating the resolution.
func resolveTranscriptPath(ctx context.Context, state *SessionState) (string, error) {
	if state.TranscriptPath == "" {
		return "", errors.New("no transcript path in session state")
	}

	// Fast path: file exists at the stored location.
	if _, err := agent.StatTranscriptFile(state.TranscriptPath); err == nil {
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

	// The stored directory is treated as the agent's session store, so the ID
	// recovered from the path is re-resolved as a name inside it rather than
	// re-joined onto it.
	store, storeErr := agent.OpenSessionStoreAt(ag, sessionDir)
	if storeErr != nil {
		return "", fmt.Errorf("transcript not found at %s: %w", state.TranscriptPath, os.ErrNotExist)
	}
	name, resolved, resolveErr := store.SessionFile(agentSessionID)
	if resolveErr != nil {
		return "", fmt.Errorf("transcript not found at %s: %w", state.TranscriptPath, os.ErrNotExist)
	}
	if resolved == state.TranscriptPath {
		// Agent resolved to the same path — not in its recorded dir.
		moved := movedTranscriptPath(ctx, ag, state, agentSessionID)
		if moved == "" {
			return "", fmt.Errorf("transcript not found at %s: %w", state.TranscriptPath, os.ErrNotExist)
		}
		logMovedTranscript(ctx, state, moved)
		state.TranscriptPath = moved
		return moved, nil
	}

	// Check if the re-resolved path exists.
	if !store.Exists(name) {
		if moved := movedTranscriptPath(ctx, ag, state, agentSessionID); moved != "" {
			resolved = moved
		} else {
			return "", fmt.Errorf("transcript not found at %s (also tried %s): %w", state.TranscriptPath, resolved, os.ErrNotExist)
		}
	}

	// Update state so subsequent reads use the correct path.
	if filepath.Dir(resolved) != sessionDir {
		logMovedTranscript(ctx, state, resolved)
	}
	state.TranscriptPath = resolved
	return resolved, nil
}

// movedTranscriptPath finds a session's transcript that its agent moved to the
// session dir of another worktree of this repository: Claude Code keys its
// session dir by working directory, so a session that moves into a worktree
// (EnterWorktree) writes its whole transcript there, and the path its hooks
// reported at session start never exists. Until its turn ends no hook reports
// the new path, so a commit made in that turn — the agent's own, typically —
// would find no transcript and no stored copy, and leave its trailer pointing
// at a checkpoint that is never written.
//
// It applies only where the recorded path is the one the agent derives for the
// session's home, so a path from attach, import or an env override is never
// second-guessed, and never to external agents, whose session-dir lookup
// starts a process. The worktree this process runs in is tried first, then
// the repository's other worktrees. The file name is the session's own ID.
func movedTranscriptPath(ctx context.Context, ag agent.Agent, state *SessionState, agentSessionID string) string {
	if state.WorktreePath == "" || external.IsExternal(ag) {
		return ""
	}
	recordedDir := filepath.Dir(state.TranscriptPath)
	if homeDir, err := ag.GetSessionDir(state.WorktreePath); err != nil || filepath.Clean(homeDir) != filepath.Clean(recordedDir) {
		return ""
	}
	current, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return ""
	}
	roots := []string{current}
	if others, err := gitrepo.ListWorktreePaths(ctx, current); err == nil {
		roots = append(roots, others...)
	}
	seen := map[string]bool{filepath.Clean(recordedDir): true}
	for _, root := range roots {
		dir, err := ag.GetSessionDir(root)
		if err != nil || seen[filepath.Clean(dir)] {
			continue
		}
		seen[filepath.Clean(dir)] = true
		store, err := agent.OpenSessionStoreAt(ag, dir)
		if err != nil {
			continue
		}
		name, resolved, err := store.SessionFile(agentSessionID)
		if err == nil && store.Exists(name) {
			return resolved
		}
	}
	return ""
}

func logMovedTranscript(ctx context.Context, state *SessionState, path string) {
	logging.Debug(logging.WithComponent(ctx, "checkpoint"), "session transcript found in another worktree's session dir",
		slog.String("session_id", state.SessionID),
		slog.String("recorded", state.TranscriptPath),
		slog.String("found", path))
}
