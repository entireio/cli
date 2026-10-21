package githelper

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// newRealGitRepo makes an empty repository for tests that drive the real git
// (handlePush spawns `git send-pack` itself) and returns its path and a runner
// for git commands in it. It isolates the process from the developer's git
// config first, because send-pack inherits the environment: a global
// push.gpgSign or core.hooksPath would otherwise hang or redirect the test.
// That mutates process-global state, so callers cannot run in parallel.
func newRealGitRepo(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	gitenv.IsolateRepository(t)
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		base := []string{"-C", dir,
			"-c", "user.name=test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}
		out, err := exec.CommandContext(context.Background(), "git", append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	return dir, run
}
