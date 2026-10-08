// hooks_claudecode_posttodo.go contains the PostTodo hook handler for Claude Code.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
)

// handleClaudeCodePostTodo handles the PostToolUse[TodoWrite] hook. It used to
// write an incremental subagent checkpoint to the session's shadow branch;
// subagent work is now captured by task records when the subagent completes,
// so the hook records nothing. It stays registered because existing
// .claude/settings.json files still invoke it.
func handleClaudeCodePostTodo(ctx context.Context) error {
	return handleClaudeCodePostTodoFromReader(ctx, os.Stdin)
}

// handleClaudeCodePostTodoFromReader consumes the hook input so the agent's
// write to the hook's stdin always completes, then does nothing.
func handleClaudeCodePostTodoFromReader(_ context.Context, reader io.Reader) error {
	if _, err := parseSubagentCheckpointHookInput(reader); err != nil {
		return fmt.Errorf("failed to parse PostToolUse[TodoWrite] input: %w", err)
	}
	return nil
}
