package gitrepo_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// gitInvocationMarkers identify a line that shells out to git, as opposed to
// the ~90 unrelated Go lines that merely contain the string "status" (JSON
// field tags, agent stream subtypes, the agent-help classification table).
var gitInvocationMarkers = []string{
	`"git"`,          // exec.Command("git", ...) and runner.RunInDir(..., "git", ...)
	"runGit",         // review's thin wrapper
	"gitexec",        // the shared git exec helper
	"RunInDir",       // bootstrapRunner
	"CommandContext", // any remaining direct spawn on the same line
}

// TestGitStatusCallSitesPassNoOptionalLocks fails the build on any `git status`
// invocation that omits --no-optional-locks.
//
// `git status` is a WRITE: it refreshes the index's stat cache and, when
// anything is stale, takes .git/index.lock for the whole worktree walk and
// renames a fresh index over .git/index. Entire only ever wants the porcelain
// output, so that write is pure collateral — and it cost a user a commit that
// deleted every tracked file (issue #2111). See the "`git status` Is a Write"
// section of CLAUDE.md for the full chain.
//
// This is a source-level guard rather than a comment on purpose. The exact same
// producer was diagnosed once before (ENT-242, Feb 2026), the fix was closed
// unmerged on the false premise that `git status --porcelain -z` "reads without
// rewriting", and the knowledge left the codebase entirely.
//
// Limitation: the check is per-line, so an invocation that gofmt wraps across
// several lines could carry the flag on a different line than "status" and read
// as a violation. That direction is safe — it fails loudly and the fix is to
// keep the argv on one line or add the flag beside "status".
func TestGitStatusCallSitesPassNoOptionalLocks(t *testing.T) {
	t.Parallel()

	root := strings.TrimSpace(testutil.RunGit(t, "", "rev-parse", "--show-toplevel"))

	// git grep exits non-zero on zero matches, so RunGit failing here means
	// "found nothing", which cannot be right — see the checked == 0 guard below.
	out := testutil.RunGit(t, root, "grep", "-n", "--", `"status"`, "--", "cmd", "internal")

	var checked int
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		path, rest, ok := strings.Cut(line, ":")
		if !ok || !strings.HasSuffix(path, ".go") {
			continue
		}
		// Test files and fixtures may assert on the unguarded argv shape.
		if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testutil/") {
			continue
		}
		if !containsAnyMarker(rest) {
			continue // not a git invocation
		}
		checked++
		if strings.Contains(rest, `"--no-optional-locks"`) {
			continue
		}
		t.Errorf("git status invocation without --no-optional-locks:\n  %s\n"+
			"`git status` rewrites the user's .git/index (issue #2111). Add "+
			`"--no-optional-locks" before "status", and set `+
			"cmd.Env = gitrepo.EnvWithoutRepoOverrides() if the call can run "+
			"inside a git hook.", line)
	}

	// If a refactor moves every call site behind a helper this pattern no longer
	// recognises, the guard would silently pass forever. Fail instead so someone
	// re-points it.
	if checked == 0 {
		t.Error("guard matched no git status invocations at all; the detection " +
			"pattern has gone stale and must be re-pointed (gitInvocationMarkers)")
	}
}

func containsAnyMarker(line string) bool {
	for _, m := range gitInvocationMarkers {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}
