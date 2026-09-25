package gitrepo_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// nativeReadGateCallers lists every non-test file that consults
// gitrepo.ReadsNeedNativeGit, with the read it gates.
//
// The gate only covers ref and object reads: it deliberately ignores
// GIT_INDEX_FILE, attributes, and pathspec variables. A new caller that reads
// any of those on the go-git path would silently read the wrong state, so
// adding an entry here is a deliberate act that should re-check that contract.
var nativeReadGateCallers = map[string]string{
	"cmd/entire/cli/gitrepo/read.go":          "the gate itself",
	"cmd/entire/cli/head_checkpoint_flags.go": "HEAD commit message",
	"cmd/entire/cli/git_operations.go":        "metadata tracking-ref tip",
	"cmd/entire/cli/strategy/common.go":       "shadow-branch existence",
}

func TestReadsNeedNativeGit_CallersAreListed(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	out := testutil.GitGrepGuard(t, repoRoot, "-l", "--fixed-strings", "--", "ReadsNeedNativeGit(", "--", ":(glob)**/*.go")
	found := map[string]bool{}
	for file := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if !strings.HasSuffix(file, ".go") {
			t.Fatalf("cannot parse git grep output; expected a .go path, got %q", file)
		}
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		found[file] = true
		if _, listed := nativeReadGateCallers[file]; !listed {
			t.Errorf("%s calls gitrepo.ReadsNeedNativeGit but is not in nativeReadGateCallers.\n"+
				"The gate covers ref and object reads only; confirm the new read does not "+
				"touch the index, attributes, or pathspecs on the go-git path, then list it. "+
				"See docs/development/git-safety.md#local-ref-and-commit-reads.", file)
		}
	}
	for file := range nativeReadGateCallers {
		if !found[file] {
			t.Errorf("%s is in nativeReadGateCallers but no longer calls the gate; remove the entry", file)
		}
	}
}
