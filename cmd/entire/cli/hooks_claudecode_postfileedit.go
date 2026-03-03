// hooks_claudecode_postfileedit.go contains the PostFileEdit hook handler for Claude Code.
// This hook tracks file edits in real-time by appending to the session's edit log
// and merging edited file paths into FilesTouched in the session state.
package cli

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// handleClaudeCodePostFileEditFromReader handles the post-file-edit hook, which fires
// after Claude Code's Write or Edit tool invocations. It tracks file edits in real-time
// for mid-turn commit attribution.
// All errors are non-fatal: logged as warnings and return nil.
//
//nolint:unparam // Always returns nil by design (non-fatal hook); error return required by hook dispatch interface.
func handleClaudeCodePostFileEditFromReader(ctx context.Context, reader io.Reader) error {
	input, err := parseFileEditHookInput(reader)
	if err != nil {
		logging.Warn(ctx, "failed to parse post-file-edit input", slog.String("error", err.Error()))
		return nil
	}

	logging.Debug(ctx, "post-file-edit",
		slog.String("hook", "post-file-edit"),
		slog.String("tool_name", input.ToolName),
		slog.String("file_path", input.FilePath),
		slog.String("session_id", input.SessionID),
	)

	// Verify session state exists before writing any files to avoid orphan edits
	state, err := strategy.LoadSessionState(ctx, input.SessionID)
	if err != nil {
		logging.Warn(ctx, "failed to load session state for file edit",
			slog.String("session_id", input.SessionID),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if state == nil {
		logging.Debug(ctx, "no active session state for file edit, skipping",
			slog.String("session_id", input.SessionID),
		)
		return nil
	}

	// Normalize path relative to repo root
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Warn(ctx, "failed to get repo root for file edit", slog.String("error", err.Error()))
		return nil
	}

	absPath := input.FilePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(repoRoot, absPath)
	}
	relPath := filepath.ToSlash(paths.ToRelativePath(absPath, repoRoot))
	if relPath == "" {
		logging.Debug(ctx, "file edit path outside repo, skipping",
			slog.String("abs_path", absPath),
			slog.String("repo_root", repoRoot),
		)
		return nil
	}

	// Determine action from tool name
	var action agent.FileEditAction
	switch input.ToolName {
	case claudecode.ToolWrite:
		action = agent.FileEditActionWrite
	case claudecode.ToolEdit:
		action = agent.FileEditActionEdit
	default:
		// Unreachable: parseFileEditHookInput rejects unsupported tools.
		// Use the tool name directly as a defensive fallback.
		action = agent.FileEditAction(input.ToolName)
	}

	edit := agent.FileEdit{
		FilePath:     relPath,
		Action:       action,
		ToolName:     input.ToolName,
		LinesAdded:   input.LinesAdded,
		LinesRemoved: input.LinesRemoved,
		Timestamp:    time.Now(),
	}

	// Append to JSONL edit log
	store, err := session.NewStateStore(ctx)
	if err != nil {
		logging.Warn(ctx, "failed to create state store for file edit", slog.String("error", err.Error()))
		return nil
	}
	if err := store.AppendFileEdit(input.SessionID, edit); err != nil {
		logging.Warn(ctx, "failed to append file edit", slog.String("error", err.Error()))
		// Continue to try updating FilesTouched even if JSONL append fails
	}

	// Merge into FilesTouched in session state
	if !slices.Contains(state.FilesTouched, relPath) {
		state.FilesTouched = append(state.FilesTouched, relPath)
		if err := strategy.SaveSessionState(ctx, state); err != nil {
			logging.Warn(ctx, "failed to save session state after file edit", slog.String("error", err.Error()))
		}
	}

	return nil
}
