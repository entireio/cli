package factoryaidroid

import (
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertion.
var _ agent.AgentHomeProvider = (*FactoryAIDroidAgent)(nil)
var _ agent.WorktreeSessionDirProvider = (*FactoryAIDroidAgent)(nil)

// SessionHome returns the home directory Droid resolves ~ to, resolved the
// same way resolveFactoryHome resolves it ($FACTORY_HOME_OVERRIDE, else the
// user's home). It intentionally does not honor ENTIRE_TEST_DROID_PROJECT_DIR:
// that override names a single project's session directory, not the home it
// would normally sit under, and GetSessionBaseDir skips it for the same
// reason.
func (f *FactoryAIDroidAgent) SessionHome() (string, error) {
	return resolveFactoryHome()
}

// SessionPathUnder reports whether path could be a Factory AI Droid session
// transcript stored beneath home. Droid writes transcripts to
// <home>/.factory/sessions/<sanitized-repo-path>/<session-id>.jsonl (see
// GetSessionDir, ResolveSessionFile), so a match requires both containment in
// <home>/.factory/sessions and a .jsonl suffix — a bare directory never
// qualifies.
func (f *FactoryAIDroidAgent) SessionPathUnder(home, path string) bool {
	if home == "" || path == "" {
		return false
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	return agent.PathHasDirPrefix(path, filepath.Join(home, ".factory", "sessions"))
}

// SessionBaseDirUnder returns the base directory containing Factory AI
// Droid's per-project session subdirectories beneath home — the same
// ".factory/sessions" suffix GetSessionBaseDir joins onto its own resolved
// home.
func (f *FactoryAIDroidAgent) SessionBaseDirUnder(home string) string {
	return filepath.Join(home, ".factory", "sessions")
}

// SessionDirUnder describes the agent's project-scoped storage layout.
func (f *FactoryAIDroidAgent) SessionDirUnder(home, worktree string) string {
	return filepath.Join(f.SessionBaseDirUnder(home), sanitizeRepoPath(worktree))
}
