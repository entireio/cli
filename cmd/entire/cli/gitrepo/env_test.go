package gitrepo

import (
	"slices"
	"strings"
	"testing"
)

func TestEnvWithoutRepoOverrides_StripsRepoSelectors(t *testing.T) {
	// Cannot be parallel: t.Setenv is process-global.
	t.Setenv("GIT_DIR", "/decoy/.git")
	t.Setenv("GIT_WORK_TREE", "/decoy")
	t.Setenv("GIT_INDEX_FILE", "/decoy/.git/index")
	t.Setenv("ENTIRE_ENV_TEST_KEEP", "keep-me")

	env := EnvWithoutRepoOverrides()

	for _, banned := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE="} {
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, banned) }) {
			t.Errorf("EnvWithoutRepoOverrides kept %s", strings.TrimSuffix(banned, "="))
		}
	}

	if !slices.Contains(env, "ENTIRE_ENV_TEST_KEEP=keep-me") {
		t.Error("EnvWithoutRepoOverrides dropped an unrelated variable; it must only " +
			"strip git's repo selectors")
	}
}

// TestEnvWithoutRepoOverrides_DoesNotStripPrefixMatches guards against a
// substring-style filter: GIT_DIRECTORY and GIT_INDEX_VERSION are not repo
// selectors and must survive. The filter matches on "NAME=" for exactly this
// reason.
func TestEnvWithoutRepoOverrides_DoesNotStripPrefixMatches(t *testing.T) {
	t.Setenv("GIT_DIRECTORY", "unrelated")
	t.Setenv("GIT_WORK_TREES_CACHE", "unrelated")

	env := EnvWithoutRepoOverrides()

	for _, want := range []string{"GIT_DIRECTORY=unrelated", "GIT_WORK_TREES_CACHE=unrelated"} {
		if !slices.Contains(env, want) {
			t.Errorf("EnvWithoutRepoOverrides stripped %q, which is not a repo selector", want)
		}
	}
}
