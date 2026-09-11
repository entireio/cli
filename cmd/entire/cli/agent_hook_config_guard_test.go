package cli

import (
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

	// Both subprocesses run through testutil.GitGrepGuard, which scrubs git's
	// repo selectors (GIT_DIR / GIT_WORK_TREE take precedence over cmd.Dir, so a
	// `go test` launched from a hook or a `git rebase --exec` resolved some other
	// repository entirely) and passes --untracked and --no-color. The first of
	// those matters more here than anywhere else: this guard compares two sets
	// built from the same grep, so a NEW agent package that calls OpenHookConfig
	// without declaring HookConfigRelPath was invisible on both sides and the
	// comparison passed — with the file unstaged, which is exactly when someone
	// is writing a new agent.
	dir, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	callers := agentPackagesMatching(t, dir, "agent.OpenHookConfig(")
	locators := agentPackagesMatching(t, dir, ") HookConfigRelPath() string {")

	// Sets, not counts. len(declared) == len(callers) passed whenever an added
	// omission was offset by a removal in the same change — the failure this
	// test exists to catch, since the agent still works and only doctor's
	// diagnosis goes quiet — and failed on an agent whose call happens to sit in
	// a sub-package, which is no defect at all. Both sides are package
	// directories, so they are directly comparable.
	require.Equal(t, locators, callers,
		"the agent packages calling agent.OpenHookConfig and those implementing\n"+
			"agent.HookConfigRelPath must be the same set. An agent that opens its\n"+
			"hook config without declaring where it lives leaves the directories\n"+
			"Entire creates between its own directory and that file unchecked by\n"+
			"doctor's symlink diagnosis.")

	// The registry is the thing doctor actually reads, so a locator that exists
	// in source but never reaches AllHookConfigRelPaths (an agent left out of
	// the registry, or one returning "") is its own failure.
	//
	// A count, deliberately, directly under the argument against counts above —
	// and defeatable the same way, by dropping one agent from the registry while
	// adding another locator. A set comparison would need to map a package
	// directory to the rel path it declares, and nothing does: `geminicli`
	// declares `.gemini/settings.json` and `copilotcli` declares
	// `.github/hooks/entire.json`, so neither the package name nor the path's
	// first component derives the other. The set comparison above is the guard
	// that matters; this one only catches a locator the registry never sees.
	require.Len(t, agent.AllHookConfigRelPaths(), len(locators),
		"%d agent packages implement HookConfigRelPath but the registry reports %d paths (%s)",
		len(locators), len(agent.AllHookConfigRelPaths()), strings.Join(agent.AllHookConfigRelPaths(), ", "))
}

// agentPackagesMatching returns the sorted, deduplicated agent package
// directories whose non-test sources contain needle, asserting that the pattern
// still matches something — a re-worded signature would otherwise turn this
// guard into a comparison of two empty sets.
func agentPackagesMatching(t *testing.T, repoRoot, needle string) []string {
	t.Helper()
	out := testutil.GitGrepGuard(t, repoRoot, "-l", "--fixed-strings", "--",
		needle, "--", ":(glob)cmd/entire/cli/agent/**/*.go")

	var pkgs []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		// A positive .go check, not just a _test.go skip. `git grep -l` emits
		// nothing but filenames and still colorizes them, so an escape-wrapped
		// path fails the _test.go suffix test, leaks test files into the set,
		// and prefixes the package directory — all while the NotEmpty assertion
		// below still passes and the two sets still compare. --no-color makes
		// that unreachable; this makes it loud if it ever becomes reachable
		// again.
		if !strings.HasSuffix(line, ".go") {
			t.Fatalf("cannot parse git grep -l output; expected a .go path, got:\n  %s\n"+
				"The filename field is unusable, so this test can prove nothing. "+
				"Check whether git is colorizing into a pipe (color.ui or color.grep set to `always`).", line)
		}
		if strings.HasSuffix(line, "_test.go") {
			continue
		}
		if dir := path.Dir(line); !slices.Contains(pkgs, dir) {
			pkgs = append(pkgs, dir)
		}
	}
	slices.Sort(pkgs)
	require.NotEmpty(t, pkgs, "the detection pattern %q has gone stale and must be re-pointed", needle)
	return pkgs
}
