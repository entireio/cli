package gitrepo_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// rawConfigReaders are the production files allowed to use go-git's
// config.Raw, each of which handles a nil Raw.
var rawConfigReaders = map[string]string{
	"cmd/entire/cli/gitrepo/raw_config.go":      "RawConfig falls back to the repository's config file",
	"cmd/entire/cli/checkpoint/objectsigner.go": "rawConfig returns Raw for system/global scope; callers nil-check it",
}

// TestConfigRawIsNotDereferencedDirectly fails on production code that uses
// go-git's config.Raw outside rawConfigReaders. Raw is nil whenever
// extensions.worktreeConfig merges a config.worktree in, and calling Section on
// it panicked every pre-push in such a repository (#3154). Read raw options
// through gitrepo.RawConfig instead.
//
// It matches any use of a Raw field, not just a call through it, so copying Raw
// into a variable first does not slip past. Method calls (`.Raw(`) are not
// config.Raw and are left out. Test code and test setup (testutil,
// integration_test) may use Raw directly; they nil-check it before writing.
func TestConfigRawIsNotDereferencedDirectly(t *testing.T) {
	t.Parallel()

	root, found := testutil.GitGrepGuardRepoRoot(t)
	if !found {
		return
	}
	// ERE has no portable \b, hence the explicit trailing class. The generated
	// API client uses go-jx's unrelated Raw type and is excluded. The allowlisted
	// files always match, so a zero-match failure means the pattern or pathspec
	// has gone stale rather than that the tree is clean.
	out := testutil.GitGrepGuard(t, root, "-n", "-E", `\.Raw([^A-Za-z0-9_(]|$)`,
		"--", ":(glob)cmd/**/*.go", ":(glob)internal/**/*.go", ":(exclude,glob)internal/coreapi/**")

	var violations []string
	seen := map[string]bool{}
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
		if _, ok := rawConfigReaders[path]; ok {
			seen[path] = true
			continue
		}
		violations = append(violations, line)
	}
	for path := range rawConfigReaders {
		if !seen[path] {
			t.Errorf("%s no longer uses config.Raw; remove it from rawConfigReaders", path)
		}
	}
	if len(violations) > 0 {
		t.Errorf("go-git's config.Raw is nil when a config.worktree is merged in; "+
			"read raw options through gitrepo.RawConfig:\n  %s", strings.Join(violations, "\n  "))
	}
}
