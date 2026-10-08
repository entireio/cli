//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// huskyH is a minimal stand-in for Husky 9.1.7's .husky/_/h (MIT,
// github.com/typicode/husky): it finds the user's hook from $0 and runs it with
// sh -e. The ~/.huskyrc and init.sh lookups are dropped to stay hermetic.
const huskyH = `#!/usr/bin/env sh
n=$(basename "$0")
s=$(dirname "$(dirname "$0")")/$n
[ ! -f "$s" ] && exit 0
[ "${HUSKY-}" = "0" ] && exit 0
sh -e "$s" "$@"
`

// huskyWrapper is what Husky 9.1.1+ writes at .husky/_/<hook>, with no
// trailing newline.
const huskyWrapper = "#!/usr/bin/env sh\n. \"$(dirname \"$0\")/h\""

// TestHuskyV9_HooksStillRunAfterEnable installs Entire through the real CLI over
// a Husky v9 layout (core.hooksPath=.husky/_) and checks the user's Husky hooks
// still decide real `git commit` and `git push`. Before the fix, Entire executed
// the backed-up wrapper, Husky looked for .husky/<hook>.pre-entire, found
// nothing and exited 0: every Husky hook silently stopped running.
func TestHuskyV9_HooksStillRunAfterEnable(t *testing.T) {
	t.Parallel()

	env := freshRepoEnv(t)
	env.SetupBareRemote()

	env.WriteFile(".husky/_/h", huskyH)
	for _, hook := range []string{"prepare-commit-msg", "commit-msg", "post-commit", "post-rewrite", "pre-push"} {
		path := filepath.Join(env.RepoDir, ".husky", "_", hook)
		if err := os.WriteFile(path, []byte(huskyWrapper), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitOutput(t, env.RepoDir, "config", "core.hooksPath", ".husky/_")
	config, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	env.AcceptGitConfigChanges(string(config))

	// --absolute-git-hook-path makes the hooks git runs invoke this build
	// rather than whatever `entire` is on PATH.
	env.RunCLI("enable", "--yes", "--telemetry=false", "--absolute-git-hook-path")

	setHuskyHook := func(hook string, exit int) {
		t.Helper()
		env.WriteFile(".husky/"+hook, fmt.Sprintf("exit %d\n", exit))
	}
	git := func(args ...string) (string, error) {
		cmd := execx.NonInteractive(t.Context(), "git", args...)
		cmd.Dir = env.RepoDir
		cmd.Env = env.cliEnv()
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	env.WriteFile("a.txt", "a\n")
	env.GitAdd("a.txt")
	setHuskyHook("commit-msg", 1)
	if out, err := git("commit", "-m", "blocked"); err == nil {
		t.Fatalf("a failing .husky/commit-msg must block git commit\n%s", out)
	}
	setHuskyHook("commit-msg", 0)
	if out, err := git("commit", "-m", "allowed"); err != nil {
		t.Fatalf("a passing .husky/commit-msg must let git commit through: %v\n%s", err, out)
	}
	setHuskyHook("pre-push", 1)
	if out, err := git("push", "origin", "HEAD"); err == nil {
		t.Fatalf("a failing .husky/pre-push must block git push\n%s", out)
	}
}
