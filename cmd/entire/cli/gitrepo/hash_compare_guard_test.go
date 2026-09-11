package gitrepo_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestHashComparisonsUseEqual fails the build on a plumbing.Hash compared with
// == or != rather than .Equal.
//
// plumbing.Hash is a struct carrying an object-format field alongside its
// bytes, so == compares that field and .Equal (bytes.Equal over the array
// alone) does not.
//
// At the pinned go-git the two cannot disagree, and that is worth stating
// plainly so nobody "proves" this guard unnecessary by observing it: every
// public constructor leaves the format unset for a 40-char hash and stamps
// "sha256" only on a 64-char one — NewHash, FromHex, ResetBySize and
// Hasher.Sum were each checked — so two hashes of the same object in one
// repository always carry the same format. That is a property of the
// dependency, pinned at an alpha, not of this code.
//
// It is also why this is a source guard and not a test. The mismatch is
// unconstructible through go-git's public API, so no behavioural test can
// distinguish the two operators: a call site reverted to == stays green. The
// convention can only be enforced where it is written.
//
// Two limitations, both deliberate:
//
//   - It catches the `.Hash ==` shape, which is every occurrence in this
//     repository today. Two local variables of type plumbing.Hash compared
//     directly (`if a == b`) need type information a textual guard does not
//     have.
//   - It skips _test.go, matching the two sibling guards in this package. Note
//     that assert.Equal on two hashes compares the format field too, via
//     reflect.DeepEqual.
//
// A second family, `== plumbing.ZeroHash`, is NOT covered here and is not the
// same severity: a 64-char zero hash carries format "sha256", so it is
// != ZeroHash while IsZero() reports true. That one is reachable in a sha256
// repository and is tracked as its own fix rather than folded in here.
func TestHashComparisonsUseEqual(t *testing.T) {
	t.Parallel()

	root, found := testutil.GitGrepGuardRepoRoot(t)
	if !found {
		return
	}

	// A literal space rather than \s: git grep -E silently matches nothing for
	// the shorthand classes, so a pattern using one reports a clean tree.
	out := testutil.GitGrepGuard(t, root, "-n", "-E", "--", `\.Hash (==|!=) `,
		"--", ":(glob)cmd/**/*.go", ":(glob)internal/**/*.go")

	var checked int
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		path, _, ok := strings.Cut(line, ":")
		if !ok || !strings.HasSuffix(path, ".go") {
			t.Fatalf("cannot parse git grep output; expected `path:line:content`, got:\n  %s", line)
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		checked++
		t.Errorf("plumbing.Hash compared with == or !=; use .Equal:\n  %s\n"+
			"== also compares the object-format field, which .Equal ignores.", line)
	}
	if checked > 0 {
		return
	}
	// Zero matches is the expected steady state here, unlike the sibling
	// guards: this one exists to keep the count at zero. Prove the pattern
	// still works instead, so a detection regression cannot masquerade as
	// success.
	probe := testutil.GitGrepGuard(t, root, "-n", "-E", "--", `\.Hash (==|!=) `,
		"--", ":(glob)cmd/**/*_test.go")
	if strings.TrimSpace(probe) == "" {
		t.Error("guard matched nothing in non-test or test sources; the detection pattern has gone stale")
	}
}
