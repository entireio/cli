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

	root, found := testutil.GitGrepGuardRepoRoot(t)
	if !found {
		return
	}

	// GitGrepGuard rather than RunGit: it passes --untracked, so a `git status`
	// call site in a not-yet-staged file is visible (new call sites arrive in new
	// files, which is the case this guard exists for), --no-color, and it scrubs
	// GIT_DIR / GIT_WORK_TREE, which RunGit's env isolation does not — it filters
	// GIT_CONFIG_* only, so a `go test` under a git hook scanned another
	// repository. It also fails loudly on zero matches, which is the same
	// staleness signal as the checked == 0 guard below.
	// Pathspec narrowed to *.go, which is what lets the parse failure below be
	// fatal: the bare `cmd internal` it used to pass also matched testdata
	// .jsonl fixtures and a .md file, so a non-.go first field was an ordinary
	// result rather than evidence of mangled output, and the skip that filtered
	// them was also the only thing standing between colorized output and a
	// guard that silently checked nothing.
	out := testutil.GitGrepGuard(t, root, "-n", "--", `"status"`,
		"--", ":(glob)cmd/**/*.go", ":(glob)internal/**/*.go")

	var checked int
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		path, rest, ok := strings.Cut(line, ":")
		// Fatal, not skip. Colorized output puts escapes in the filename field,
		// so every line failed this check, `checked` stayed 0, and the guard
		// reported its detection pattern stale — the #2248 misdiagnosis, from a
		// tree that was perfectly fine.
		if !ok || !strings.HasSuffix(path, ".go") {
			t.Fatalf("cannot parse git grep output; expected `path:line:content`, got:\n  %s\n"+
				"The filename field is unusable, so this test can prove nothing. "+
				"Check whether git is colorizing into a pipe (color.ui or color.grep set to `always`).", line)
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

type safeGitDiffCall struct {
	path     string
	fragment string
	reason   string
}

// safeGitDiffCalls is an explicit file-and-shape allowlist. It is a slice so
// one file can contain several independently justified "diff" literals.
// Matching only the line containing "diff" is intentional: a wrapped argv
// separates that line from exec.Command, so requiring an invocation marker
// would make the guard silently miss exactly the call it exists to prevent.
var safeGitDiffCalls = []safeGitDiffCall{
	{
		path:     "cmd/entire/cli/experts_cmd.go",
		fragment: `"--cached"`,
		reason:   "compares the index to HEAD and never reads the worktree",
	},
	{
		path:     "cmd/entire/cli/review/scope.go",
		fragment: `baseRef+"...HEAD"`,
		reason:   "compares two commits and never reads the worktree",
	},
	{
		path:     "cmd/entire/cli/strategy/manual_commit_hooks.go",
		fragment: `"--cached"`,
		reason:   "compares the index to HEAD and never reads the worktree",
	},
}

// TestGitWorktreeDiffCallSitesDoNotRefreshTheIndex prevents native `git diff
// <tree> -- <paths>` from entering hook paths. Unlike `git status`, Git's diff
// builtin refreshes and rewrites a stat-stale index even under
// --no-optional-locks. Index-only and two-tree diffs are allowlisted above.
func TestGitWorktreeDiffCallSitesDoNotRefreshTheIndex(t *testing.T) {
	t.Parallel()

	root, found := testutil.GitGrepGuardRepoRoot(t)
	if !found {
		return
	}
	out := testutil.GitGrepGuard(t, root, "-n", "--", `"diff"`,
		"--", ":(glob)cmd/**/*.go", ":(glob)internal/**/*.go")

	seen := make([]bool, len(safeGitDiffCalls))
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		path, rest, ok := strings.Cut(line, ":")
		if !ok || !strings.HasSuffix(path, ".go") {
			t.Fatalf("cannot parse git grep output; expected `path:line:content`, got:\n  %s", line)
		}
		if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testutil/") {
			continue
		}
		matched := false
		for i, allowed := range safeGitDiffCalls {
			if path == allowed.path && strings.Contains(rest, allowed.fragment) {
				seen[i] = true
				matched = true
			}
		}
		if !matched {
			t.Errorf("worktree-comparing git diff can rewrite the index even with --no-optional-locks:\n  %s\n"+
				"Use a non-refreshing primitive such as git hash-object or git diff-index, or add a justified "+
				"allowlist entry if this line is not a worktree-comparing Git invocation.", line)
		}
	}
	for i, allowed := range safeGitDiffCalls {
		if !seen[i] {
			t.Errorf("safe git diff allowlist entry is stale: %s containing %q (%s)",
				allowed.path, allowed.fragment, allowed.reason)
		}
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
