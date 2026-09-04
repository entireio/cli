package cli

import (
	"os/exec"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/stretchr/testify/require"
)

// TestAllHookConfigRelPaths_CoversEveryWorktreeConfigAgent fails the build when
// an agent opens a hook-config file without also declaring where it lives.
//
// A source-level guard rather than a comment, because the omission is invisible
// at the call site: the agent works, its hooks install, and the only thing that
// silently loses coverage is doctor's symlink diagnosis — which reports nothing
// rather than reporting a problem, so no one finds out. Pi and OpenCode nest
// their config below the directory ProtectedDirs names, and the levels in
// between went unchecked exactly this way.
//
// It runs in package cli because that is where every agent's init() has run and
// the registry is fully populated.
func TestAllHookConfigRelPaths_CoversEveryWorktreeConfigAgent(t *testing.T) {
	t.Parallel()

	// Both subprocesses run with git's repo selectors scrubbed. They inherit the
	// environment, and GIT_DIR / GIT_WORK_TREE take precedence over both the
	// working directory and grep.Dir — so a `go test` launched from anywhere
	// that exports them (a `git rebase --exec`, a hook, this CLI's own test
	// harnesses) resolved some other repository entirely. Measured against a
	// decoy repo, the unscrubbed version fails with "no agent calls
	// agent.OpenHookConfig", which is a guard failing for a reason that has
	// nothing to do with what it guards; a decoy that happened to contain
	// matching paths would instead have it pass having checked nothing.
	topLevel := exec.Command("git", "rev-parse", "--show-toplevel") //nolint:noctx // guard test, no cancellation needed
	topLevel.Env = gitrepo.EnvWithoutRepoOverrides()
	repoRoot, err := topLevel.Output()
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
	}

	grep := exec.Command("git", "grep", "-l", "--fixed-strings", "--", //nolint:noctx // guard test, no cancellation needed
		"agent.OpenHookConfig(", "--", ":(glob)cmd/entire/cli/agent/**/*.go")
	grep.Dir = strings.TrimSpace(string(repoRoot))
	grep.Env = gitrepo.EnvWithoutRepoOverrides()
	out, grepErr := grep.Output()
	require.NoError(t, grepErr, "no agent calls agent.OpenHookConfig, which cannot be right")

	callers := make(map[string]struct{})
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.HasSuffix(line, "_test.go") {
			continue
		}
		callers[path.Dir(line)] = struct{}{}
	}
	require.NotEmpty(t, callers, "the detection pattern has gone stale and must be re-pointed")

	declared := agent.AllHookConfigRelPaths()
	require.Len(t, declared, len(callers),
		"%d agent packages call agent.OpenHookConfig (%s) but %d declare a path (%s).\n"+
			"Every agent whose hook config is a worktree file must implement "+
			"agent.HookConfigLocator, or the directories Entire creates between its "+
			"own directory and that file go unchecked by doctor's symlink diagnosis.",
		len(callers), strings.Join(sortedPackageDirs(callers), ", "), len(declared), strings.Join(declared, ", "))
}

func sortedPackageDirs(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
