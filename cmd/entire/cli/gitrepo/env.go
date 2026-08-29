package gitrepo

import (
	"os"
	"strings"
)

// repoOverrideEnvVars are git's repo-selector environment variables. Git
// exports them to its hooks, and they take precedence over a child process's
// working directory — so `exec.Command("git", ...)` with cmd.Dir set still
// resolves the *hook's* repository, not the directory named, and
// GIT_INDEX_FILE redirects index reads and writes to a different file
// entirely.
var repoOverrideEnvVars = []string{
	"GIT_DIR=",
	"GIT_WORK_TREE=",
	"GIT_INDEX_FILE=",
}

// EnvWithoutRepoOverrides returns the current environment minus git's
// repo-selector variables (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE), so a git
// subprocess resolves its repository from cmd.Dir as the call site intends.
//
// Use this for any git subprocess that can run inside a git hook and that
// names its target with cmd.Dir or `-C`. Inheriting these variables makes the
// child silently operate on the hook's repository instead: `git -C <other>
// rev-parse` reports the hook's repo, and an index-touching command reads and
// writes whatever GIT_INDEX_FILE names.
//
// Deliberately not applied to user-invoked commands that operate on the
// current directory (`entire status`, `entire doctor`, `entire review`): there
// a GIT_DIR the user exported in their own shell is an instruction, not
// contamination.
func EnvWithoutRepoOverrides() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		if hasAnyPrefix(kv, repoOverrideEnvVars) {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
