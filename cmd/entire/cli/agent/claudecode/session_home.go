package claudecode

import (
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertion.
var _ agent.AgentHomeProvider = (*ClaudeCodeAgent)(nil)
var _ agent.WorktreeSessionDirProvider = (*ClaudeCodeAgent)(nil)

// SessionHome returns Claude Code's per-user configuration directory,
// resolved the same way GetSessionDir resolves it (probe first, then
// $CLAUDE_CONFIG_DIR, then ~/.claude). It intentionally does not honor
// ENTIRE_TEST_CLAUDE_PROJECT_DIR: that override names a single project's
// session directory, not the home transcripts live under, and GetSessionBaseDir
// skips it for the same reason.
func (c *ClaudeCodeAgent) SessionHome() (string, error) {
	return resolveClaudeConfigDir()
}

// SessionPathUnder reports whether path could be a Claude Code session
// transcript stored beneath home. Claude writes transcripts to
// <home>/projects/<sanitized-project-dir>/<session-id>.jsonl, so a match
// requires both containment in <home>/projects and a .jsonl suffix — a bare
// directory (including <home>/projects itself) never qualifies.
func (c *ClaudeCodeAgent) SessionPathUnder(home, path string) bool {
	if home == "" || path == "" {
		return false
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	return agent.PathHasDirPrefix(path, filepath.Join(home, "projects"))
}

// SessionBaseDirUnder returns the base directory containing Claude Code's
// per-project session subdirectories beneath home — the same "projects"
// suffix GetSessionBaseDir joins onto its own resolved configuration
// directory.
func (c *ClaudeCodeAgent) SessionBaseDirUnder(home string) string {
	return filepath.Join(home, "projects")
}

// SessionDirUnder describes the agent's project-scoped storage layout.
func (c *ClaudeCodeAgent) SessionDirUnder(home, worktree string) string {
	return filepath.Join(c.SessionBaseDirUnder(home), SanitizePathForClaude(worktree))
}
