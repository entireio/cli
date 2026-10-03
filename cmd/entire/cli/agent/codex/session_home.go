package codex

import (
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertion.
var _ agent.AgentHomeProvider = (*CodexAgent)(nil)

// SessionHome returns Codex's home directory, resolved the same way
// GetSessionDir resolves it ($CODEX_HOME, else ~/.codex). It intentionally
// does not honor ENTIRE_TEST_CODEX_SESSION_DIR: that override names the
// sessions directory directly, not the home it would normally sit under.
func (c *CodexAgent) SessionHome() (string, error) {
	return resolveCodexHome()
}

// SessionPathUnder reports whether path could be a Codex session rollout
// stored beneath home. Codex writes live rollouts to
// <home>/sessions/YYYY/MM/DD/rollout-...-<id>.jsonl and, once rotated,
// <home>/archived_sessions/YYYY/MM/DD/rollout-...-<id>.jsonl (see
// rolloutRoots). A match requires containment in one of those two roots and
// a .jsonl suffix — a bare directory never qualifies.
func (c *CodexAgent) SessionPathUnder(home, path string) bool {
	if home == "" || path == "" {
		return false
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	return agent.PathHasDirPrefix(path, filepath.Join(home, "sessions")) ||
		agent.PathHasDirPrefix(path, filepath.Join(home, "archived_sessions"))
}

// SessionBaseDirUnder returns the directory containing Codex's live rollouts
// beneath home. Codex does not implement SessionBaseDirProvider — rollouts
// are organized by date, not by project, so there is no per-project
// subdirectory layout to walk. Attach's fallback search instead resolves a
// session ID directly inside this directory for each recorded home.
func (c *CodexAgent) SessionBaseDirUnder(home string) string {
	return filepath.Join(home, "sessions")
}
