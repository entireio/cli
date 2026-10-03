package pi

import (
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertion.
var _ agent.AgentHomeProvider = (*PiAgent)(nil)
var _ agent.WorktreeSessionDirProvider = (*PiAgent)(nil)

// SessionHome returns Pi's home directory, resolved the same way
// resolvePiHome resolves it ($PI_CODING_AGENT_DIR, else ~/.pi/agent). It
// intentionally does not honor PI_CODING_AGENT_SESSION_DIR or
// ENTIRE_TEST_PI_SESSION_DIR: both of those replace GetSessionDir's answer
// outright, with no per-user-home-rooted layout underneath — there is no
// home-relative session path for SessionPathUnder to verify in that case, so
// this capability only covers Pi's default, home-rooted session store.
func (a *PiAgent) SessionHome() (string, error) {
	return resolvePiHome()
}

// SessionPathUnder reports whether path could be a Pi session transcript
// stored beneath home. Pi writes transcripts to
// <home>/sessions/<encoded-repo-path>/<timestamp>_<id>.jsonl (see
// GetSessionDir, ResolveSessionFile), so a match requires both containment in
// <home>/sessions and a .jsonl suffix — a bare directory never qualifies.
func (a *PiAgent) SessionPathUnder(home, path string) bool {
	if home == "" || path == "" {
		return false
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	return agent.PathHasDirPrefix(path, filepath.Join(home, "sessions"))
}

// SessionBaseDirUnder returns the base directory containing Pi's per-project
// session subdirectories beneath home — the same "sessions" suffix
// GetSessionBaseDir joins onto its own resolved home in the default,
// home-rooted case. Like SessionHome, this does not honor
// PI_CODING_AGENT_SESSION_DIR: that override replaces the session store with
// a flat directory not rooted at any home, so there is no home-relative base
// dir for it to compute.
func (a *PiAgent) SessionBaseDirUnder(home string) string {
	return filepath.Join(home, "sessions")
}

// SessionDirUnder describes the agent's project-scoped storage layout.
func (a *PiAgent) SessionDirUnder(home, worktree string) string {
	return filepath.Join(a.SessionBaseDirUnder(home), encodeRepoPathForPi(worktree))
}
