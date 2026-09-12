package gitrepo

import "github.com/entireio/cli/cmd/entire/cli/execx"

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
	return execx.EnvWithoutRepoOverrides()
}
