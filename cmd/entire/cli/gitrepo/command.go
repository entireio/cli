package gitrepo

import (
	"context"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// worktreeGitCommand builds a git subprocess that targets worktreeRoot with
// -C, ignores repository selectors exported by a hook's environment
// (EnvWithoutRepoOverrides), and really dies when ctx is cancelled: these run
// on agent hook paths, where a hung clean filter or `git check-ignore` must
// not outlive the hook's budget (execx.TerminateOnCancel kills the process
// group and force-closes the pipes).
func worktreeGitCommand(ctx context.Context, worktreeRoot string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", worktreeRoot}, args...)...)
	cmd.Env = EnvWithoutRepoOverrides()
	execx.TerminateOnCancel(cmd)
	return cmd
}
