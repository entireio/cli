package copilotcli

import (
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertion.
var _ agent.AgentHomeProvider = (*CopilotCLIAgent)(nil)

// SessionHome returns Copilot CLI's home directory, resolved the same way
// GetSessionDir resolves it ($COPILOT_HOME, else ~/.copilot). It
// intentionally does not honor ENTIRE_TEST_COPILOT_SESSION_DIR: that override
// names the session-state directory directly, not the home it normally sits
// under.
func (c *CopilotCLIAgent) SessionHome() (string, error) {
	return resolveCopilotHome()
}

// SessionPathUnder reports whether path could be a Copilot CLI session
// transcript stored beneath home. Copilot writes transcripts to
// <home>/session-state/<session-id>/events.jsonl (see ResolveSessionFile), so
// a match requires both containment in <home>/session-state and a base name
// of exactly "events.jsonl" — a bare directory never qualifies.
func (c *CopilotCLIAgent) SessionPathUnder(home, path string) bool {
	if home == "" || path == "" {
		return false
	}
	if filepath.Base(path) != "events.jsonl" {
		return false
	}
	return agent.PathHasDirPrefix(path, filepath.Join(home, "session-state"))
}

// SessionBaseDirUnder returns the directory containing Copilot CLI's session
// state beneath home. Copilot CLI does not implement SessionBaseDirProvider:
// transcripts live at <home>/session-state/<session-id>/events.jsonl, keyed
// directly by session ID with no per-project subdirectory layer the way
// Claude Code's and Pi's project dirs are — GetSessionDir plus
// ResolveSessionFile already resolve a session ID to a path with no
// discovery walk needed, so there is nothing for attach's cross-project
// fallback search to walk here. That search instead resolves a session ID
// directly inside this directory for each recorded home.
func (c *CopilotCLIAgent) SessionBaseDirUnder(home string) string {
	return filepath.Join(home, "session-state")
}
