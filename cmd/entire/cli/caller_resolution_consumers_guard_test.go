package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// callerResolutionConsumers is every file that obtains a caller resolution,
// with what it does about ResolvedSession.Incomplete.
//
// Identification and completeness answer different questions and neither
// implies the other: IsCaller rests on "no session nearer than this one
// exists", which is precisely the claim a candidate missing from the listing
// invalidates. A listing is not fatal on a state file it cannot read — one
// corrupt file must not blind every command to the rest of the store — so the
// loss arrives as a SUCCESSFUL resolution with a candidate quietly absent.
// Every consumer therefore has to decide, and the two defensible answers split
// by what the consumer does with the session: acting on one refuses, merely
// displaying one says so and carries on.
//
// A ledger rather than a doc comment because `ResolvedSession.Incomplete`
// already carries that rule in prose and `checkpoint create` was added as an
// IsCaller consumer without it anyway — it checked Found, IsCaller and
// Tracked, and would create a checkpoint on whichever session won an
// uncontested environment claim. Prose cannot fail when someone adds a
// consumer.
var callerResolutionConsumers = map[string]string{
	"cmd/entire/cli/checkpoint_create.go": "refuses: Incomplete is tested ahead of Found and IsCaller, and an explicit session ID bypasses resolution entirely",
	"cmd/entire/cli/session_adopt.go":     "refuses: adoptSourceIsOwned orders caller.Incomplete ahead of every verdict, and --allow-foreign-session is the override",
	"cmd/entire/cli/session_current.go":   "warns on stderr, before the not-found branch; it reports a session rather than acting on one",
	"cmd/entire/cli/session_tokens.go":    "warns on stderr for both selection paths, before the not-found return; it reports rather than acts",
	"cmd/entire/cli/hooks_git_cmd.go":     "ignores it deliberately: the resolved ID only stamps a log field, and the store's per-skip Warn lands in that same log",
}

// TestCallerResolutionConsumersHandleIncompleteness fails the build when a new
// caller resolution appears without an entry above, when an entry outlives the
// call it describes, and when a consumer does not name Incomplete at all.
//
// It cannot verify that a stated reason is TRUE — that is the reviewer's job,
// and writing the reason down is what gives them something to check. What it
// guarantees is that nobody obtains a resolution without being asked the
// question, including the consumer that answers "ignore it", which has to say
// so where the next reader will look.
func TestCallerResolutionConsumersHandleIncompleteness(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	// The files that DEFINE these entry points are not consumers of them. Any
	// other file in the strategy package that resolves a caller still has to
	// be audited, which is why this skips two paths rather than the package.
	definitions := map[string]struct{}{
		"cmd/entire/cli/strategy/caller_session.go": {},
		"cmd/entire/cli/strategy/session_state.go":  {},
	}

	found := map[string]struct{}{}
	for _, needle := range []string{
		"ResolveCallerSession(",
		"IdentifyCallerSession(",
		"FindMostRecentSessionInCurrentWorktree(",
	} {
		out := testutil.GitGrepGuard(t, repoRoot, "-l", "--fixed-strings", "--", needle,
			"--", ":(glob)**/*.go", ":(exclude,glob)**/*_test.go")
		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			if !strings.HasSuffix(line, ".go") {
				t.Fatalf("cannot parse git grep -l output; expected a .go path, got:\n  %s", line)
			}
			if _, isDefinition := definitions[line]; isDefinition {
				continue
			}
			found[line] = struct{}{}
		}
	}
	if len(found) == 0 {
		t.Fatal("guard matched no consumers at all; the detection pattern has gone stale")
	}

	for file := range found {
		if _, audited := callerResolutionConsumers[file]; !audited {
			t.Errorf("%s obtains a caller resolution but is not in callerResolutionConsumers.\n"+
				"A resolution can be returned over a candidate set known to be missing entries, and "+
				"IsCaller does not fold that in — anything that ACTS on the session must refuse "+
				"(ResolvedSession.Incomplete != nil), and anything that merely reports one must say "+
				"so. Decide which, then add an entry saying so.", file)
			continue
		}
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(body), "Incomplete") {
			t.Errorf("%s is in callerResolutionConsumers but never names Incomplete.\n"+
				"Even the consumer that deliberately ignores it has to say so in a comment, or the "+
				"next reader cannot tell a decision from an oversight.", file)
		}
	}
	for file := range callerResolutionConsumers {
		if _, still := found[file]; !still {
			t.Errorf("%s is in callerResolutionConsumers but no longer obtains a caller "+
				"resolution; remove the entry", file)
		}
	}
}
