package gitrepo

import (
	"os"
	"strings"
)

// Inherited repository selectors can redirect Git even when cmd.Dir is explicit.
// GIT_COMMON_DIR redirects shared repository data, including configuration;
// GIT_INDEX_FILE redirects index reads and writes.
var repoOverrideEnvVars = []string{
	"GIT_DIR=",
	"GIT_COMMON_DIR=",
	"GIT_WORK_TREE=",
	"GIT_INDEX_FILE=",
}

// EnvWithoutRepoOverrides returns the current environment minus git's
// repo-selector variables (GIT_DIR, GIT_COMMON_DIR, GIT_WORK_TREE, GIT_INDEX_FILE),
// so a git subprocess resolves its repository from cmd.Dir as the call site intends.
//
// Use this for git subprocesses that must resolve their target independently
// of the enclosing hook, using cmd.Dir or `-C`. Inheriting these variables makes the
// child silently operate on the hook's repository instead: `git -C <other>
// rev-parse` reports the hook's repo, and an index-touching command reads and
// writes whatever GIT_INDEX_FILE names.
//
// Commands inspecting the commit being prepared must retain Git's temporary
// GIT_INDEX_FILE rather than use this helper.
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
