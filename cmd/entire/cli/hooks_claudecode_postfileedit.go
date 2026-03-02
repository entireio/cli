// hooks_claudecode_postfileedit.go contains the PostFileEdit hook handler for Claude Code.
// This hook tracks file edits in real-time by appending to the session's edit log
// and merging edited file paths into FilesTouched in the session state.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// handleClaudeCodePostFileEdit handles the PostToolUse[Write|Edit] hook.
// It tracks file edits in real-time for mid-turn commit attribution.
// All errors are non-fatal: logged as warnings and return nil.
func handleClaudeCodePostFileEdit(ctx context.Context) error {
	return handleClaudeCodePostFileEditFromReader(ctx, os.Stdin)
}

// handleClaudeCodePostFileEditFromReader is the testable version that accepts an io.Reader.
func handleClaudeCodePostFileEditFromReader(ctx context.Context, reader io.Reader) error {
	input, err := parseFileEditHookInput(reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entire] warning: failed to parse post-file-edit input: %v\n", err)
		return nil
	}

	logging.Debug(ctx, "post-file-edit",
		slog.String("hook", "post-file-edit"),
		slog.String("tool_name", input.ToolName),
		slog.String("file_path", input.FilePath),
		slog.String("session_id", input.SessionID),
	)

	// Normalize path relative to repo root
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entire] warning: failed to get repo root: %v\n", err)
		return nil
	}

	absPath := input.FilePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(repoRoot, absPath)
	}
	relPath := paths.ToRelativePath(absPath, repoRoot)
	if relPath == "" {
		// Path is outside repo, skip silently
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
		fmt.Fprintf(os.Stderr, "[entire] warning: failed to create state store: %v\n", err)
		return nil
	}
	if err := store.AppendFileEdit(input.SessionID, edit); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] warning: failed to append file edit: %v\n", err)
		// Continue to try updating FilesTouched even if JSONL append fails
	}

	// Merge into FilesTouched in session state
	state, err := strategy.LoadSessionState(ctx, input.SessionID)
	if err != nil {
		// Load error - not fatal, skip FilesTouched update
		return nil //nolint:nilerr // non-fatal: don't block agent on state load failure
	}
	if state == nil {
		// No active session state - not fatal
		return nil
	}
	if !slices.Contains(state.FilesTouched, relPath) {
		state.FilesTouched = append(state.FilesTouched, relPath)
		if err := strategy.SaveSessionState(ctx, state); err != nil {
			fmt.Fprintf(os.Stderr, "[entire] warning: failed to save session state: %v\n", err)
		}
	}

	return nil
}
