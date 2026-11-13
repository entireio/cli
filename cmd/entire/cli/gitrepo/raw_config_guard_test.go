package gitrepo_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestConfigRawIsNotDereferencedDirectly fails on production code that calls
// through go-git's config.Raw. Raw is nil whenever extensions.worktreeConfig
// merges a config.worktree in, and calling Section on it panicked every
// pre-push in such a repository (#3154). Read raw options through
// gitrepo.RawConfig instead.
//
// Test code and test setup (testutil, integration_test) may use Raw directly;
// they nil-check it before writing.
func TestConfigRawIsNotDereferencedDirectly(t *testing.T) {
	t.Parallel()

	root, found := testutil.GitGrepGuardRepoRoot(t)
	if !found {
		return
	}
	// The test helpers always match, so a zero-match failure means the pattern
	// or pathspec has gone stale rather than that the tree is clean.
	out := testutil.GitGrepGuard(t, root, "-n", "-E", `\.Raw\.`,
		"--", ":(glob)cmd/**/*.go", ":(glob)internal/**/*.go")

	var violations []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		path, _, ok := strings.Cut(line, ":")
		if !ok || !strings.HasSuffix(path, ".go") {
			t.Fatalf("cannot parse git grep output; expected `path:line:content`, got:\n  %s", line)
		}
		if strings.HasSuffix(path, "_test.go") ||
			strings.Contains(path, "/testutil/") ||
			strings.Contains(path, "/integration_test/") {
			continue
		}
		violations = append(violations, line)
	}
	if len(violations) > 0 {
		t.Errorf("go-git's config.Raw is nil when a config.worktree is merged in; "+
			"read raw options through gitrepo.RawConfig:\n  %s", strings.Join(violations, "\n  "))
	}
}
