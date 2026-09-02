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

	dir := strings.TrimSpace(string(repoRoot))
	callers := agentPackagesMatching(t, dir, "agent.OpenHookConfig(")
	require.NotEmpty(t, callers, "the detection pattern has gone stale and must be re-pointed")
	locators := agentPackagesMatching(t, dir, ") HookConfigRelPath() string {")
	require.NotEmpty(t, locators, "the detection pattern has gone stale and must be re-pointed")

	// Sets, not counts. len(declared) == len(callers) passed whenever an added
	// omission was offset by a removal in the same change — the failure this
	// test exists to catch, since the agent still works and only doctor's
	// diagnosis goes quiet — and failed on an agent whose call happens to sit in
	// a sub-package, which is no defect at all. Both sides are package
	// directories, so they are directly comparable.
	require.Equal(t, sortedPackageDirs(locators), sortedPackageDirs(callers),
		"the agent packages calling agent.OpenHookConfig and those implementing\n"+
			"agent.HookConfigRelPath must be the same set. An agent that opens its\n"+
			"hook config without declaring where it lives leaves the directories\n"+
			"Entire creates between its own directory and that file unchecked by\n"+
			"doctor's symlink diagnosis.")

	// The registry is the thing doctor actually reads, so a locator that exists
	// in source but never reaches AllHookConfigRelPaths (an agent left out of
	// the registry, or one returning "") is its own failure.
	require.Len(t, agent.AllHookConfigRelPaths(), len(locators),
		"%d agent packages implement HookConfigRelPath but the registry reports %d paths (%s)",
		len(locators), len(agent.AllHookConfigRelPaths()), strings.Join(agent.AllHookConfigRelPaths(), ", "))
}

// agentPackagesMatching returns the agent package directories whose non-test
// sources contain needle.
func agentPackagesMatching(t *testing.T, repoRoot, needle string) map[string]struct{} {
	t.Helper()
	grep := exec.Command("git", "grep", "-l", "--fixed-strings", "--", //nolint:noctx // guard test, no cancellation needed
		needle, "--", ":(glob)cmd/entire/cli/agent/**/*.go")
	grep.Dir = repoRoot
	// Set for the same reason every git subprocess naming its target with
	// cmd.Dir does: git exports GIT_DIR/GIT_WORK_TREE to hooks, and those take
	// precedence over cmd.Dir. Came from main while this test was being
	// rewritten; kept.
	grep.Env = gitrepo.EnvWithoutRepoOverrides()
	out, err := grep.Output()
	require.NoError(t, err, "no agent source matches %q, which cannot be right", needle)

	pkgs := make(map[string]struct{})
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.HasSuffix(line, "_test.go") {
			continue
		}
		pkgs[path.Dir(line)] = struct{}{}
	}
	return pkgs
}

func sortedPackageDirs(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
