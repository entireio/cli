package testutil

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// GitGrepGuard runs `git grep` for a source-level guard test and returns its
// output, failing the test when the pattern matched nothing.
//
// It exists because this repo has four guards that scan their own source with
// `git grep` — the root-base allowlist, the transcript-read ratchet, the
// `git status` flag check, and the agent hook-config coverage check — and
// getting the invocation right is not obvious. Each of the three flags below
// was missing from at least one of them, and the same bug was found in three:
// two copies is how they drift, and four is how they drifted.
//
// --untracked, because git grep otherwise searches only the INDEX. Every one of
// these guards has "someone just added a file" as its subject, so the file it
// most needs to see is the one not yet staged. The hole is worse than a plain
// miss in two of them: the hook-config guard compares two sets built from the
// same blind grep, so a new agent missing its locator is invisible on both
// sides and the comparison passes; and the root-base guard's staleness half
// reported a just-added entry as STALE, telling the author to delete the entry
// that legitimises their new root. Ignored files stay excluded, which is right.
//
// --no-color, because `color.ui` or `color.grep` set to `always` colorizes even
// into a pipe, and the escapes land in the FILENAME field. Confirmed for both
// output shapes: `-n` gives `\e[35mpath\e[m\e[36m:\e[m...`, and `-l` — which
// looks like it should be immune, being nothing but filenames — gives
// `\e[35mpath\e[m`. Every one of these guards then either misparses (a `.go`
// suffix check fails and the guard reports itself stale, which was read as a
// real staleness failure on main, #2248) or, worse, silently compares garbage
// to garbage while its "did we match anything" assertion still passes.
//
// Repo-selector scrubbing, because git exports GIT_DIR / GIT_WORK_TREE to the
// hooks it runs and those take precedence over cmd.Dir. A `go test` launched
// from anywhere that exports them — a `git rebase --exec`, a hook, this CLI's
// own harnesses — pointed the grep at some other repository. Measured against a
// decoy repo, an unscrubbed grep returns the decoy's files while cmd.Dir names
// this one. GIT_CONFIG_* isolation is applied too (gitenv.Isolated), so a
// developer's global grep settings cannot change the pattern semantics either.
//
// args are the caller's own flags, pattern and pathspecs, appended after the
// three above.
func GitGrepGuard(t *testing.T, repoRoot string, args ...string) string {
	t.Helper()

	full := append([]string{"grep", "--untracked", "--no-color"}, args...)
	cmd := exec.Command("git", full...) //nolint:noctx // guard test, no cancellation needed
	cmd.Dir = repoRoot
	cmd.Env = GuardGitEnv()
	out, err := cmd.Output()
	if err != nil {
		// git grep exits non-zero on zero matches, so this is "found nothing",
		// which for a guard means its detection pattern has gone stale rather
		// than that the tree is clean.
		t.Fatalf("git grep %v found nothing, which cannot be right — the detection pattern has gone stale: %v", args, err)
	}
	return string(out)
}

// GitGrepGuardRepoRoot resolves the repository root for a guard test, skipping
// when there is no checkout to scan.
func GitGrepGuardRepoRoot(t *testing.T) (string, bool) {
	t.Helper()

	cmd := exec.Command("git", "rev-parse", "--show-toplevel") //nolint:noctx // guard test, no cancellation needed
	cmd.Env = GuardGitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// GuardGitEnv drops the repo selectors and the caller's git config, keeping
// both isolations in one place so the two subprocesses above cannot disagree
// about which repository they are looking at.
func GuardGitEnv() []string {
	scrubbed := gitrepo.EnvWithoutRepoOverrides()
	var kept []string
	for _, kv := range scrubbed {
		if strings.HasPrefix(kv, "GIT_CONFIG_") {
			continue
		}
		kept = append(kept, kv)
	}
	return append(kept, gitenv.EmptyConfigOverrides()...)
}
