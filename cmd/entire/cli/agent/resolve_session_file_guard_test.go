package agent_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// resolveSessionFilePattern matches a call to an agent's own
// ResolveSessionFile or ResolveRestoredSessionFile. The leading `\.` is what
// keeps a method DEFINITION (`) ResolveSessionFile(`) out: definitions have no
// dot before the name, so only call sites match.
//
// Both resolvers, because both turn an agent-supplied ID into a path from a
// directory the caller passes in, and the restored one additionally derives its
// answer from checkpoint transcript bytes.
const resolveSessionFilePattern = `\.Resolve[A-Za-z]*SessionFile\(`

// resolveSessionFileCallers is every file allowed to call an agent's
// ResolveSessionFile directly, with the reason it may.
//
// The method takes an agentSessionID that reached us from a hook payload or
// from checkpoint metadata on the shared entire/checkpoints/v1 branch, and
// several agents use it as a DIRECTORY component (Copilot:
// <dir>/<id>/events.jsonl) or return it verbatim when absolute (Codex, Pi). Its
// doc comment therefore says not to call it with unvalidated input — an
// invariant the compiler cannot check, and one that two agents violated for as
// long as it existed, because a new integration copies the nearest existing one
// rather than re-deriving whether the ID was checked.
//
// The fix for a new violation is not an entry here: it is
// SessionStore.SessionFile, which validates the ID and confirms the resolved
// path is inside the store, and which is what both former violators now use.
var resolveSessionFileCallers = map[string]string{
	"cmd/entire/cli/agent/session_store.go": "SessionFile itself — the chokepoint that validates the ID before resolving it, and converts the result back into a name inside the store",

	// Calls ResolveRestoredSessionFile with an ID SessionFile has already
	// validated, and passes the result back through SessionStore.Name before
	// using it.
	"cmd/entire/cli/strategy/manual_commit_pending.go": "restore path: resolves an already-validated ID and re-checks containment through the store",

	// Pure delegation across the external-plugin boundary: the wrapper forwards
	// to the plugin's implementation and resolves nothing of its own, so it is
	// the method rather than a caller of it.
	"cmd/entire/cli/agent/external/capabilities.go": "wrappedAgent forwards the call to the external agent; it is an implementation, not a caller",

	// A test helper, in a package with no _test.go suffix for the exclusion to
	// catch. It resolves an ID the harness itself wrote into a temp repo, so
	// there is no untrusted input; listed rather than excluded by path so that a
	// production caller added under e2e/ cannot hide behind the exclusion.
	"e2e/testutil/session_paths.go": "e2e harness resolving an ID it generated itself, against a temp repo",
}

// TestResolveSessionFileCallersAreSanctioned fails the build when a new caller
// of ResolveSessionFile appears, and when a sanctioned one stops calling it.
//
// Both directions, for the same reason as the transcript-read ratchet: growth
// is the regression, and a stale entry makes the list stop meaning anything.
func TestResolveSessionFileCallersAreSanctioned(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	// testutil.GitGrepGuard owns --untracked, --no-color and the repo-selector
	// scrubbing, and explains why each is load-bearing for a guard like this.
	// The pathspec is restricted to *.go so an unparseable path can be fatal
	// below rather than skipped, and spans EVERY Go package rather than cmd/:
	// scoped to cmd/**/*.go it could not see e2e/testutil, which had been
	// calling the resolver the whole time. A guard that cannot see a caller is
	// not a guard.
	out := testutil.GitGrepGuard(t, repoRoot, "-l", "-E", "--", resolveSessionFilePattern,
		"--", ":(glob)**/*.go", ":(exclude,glob)**/*_test.go")

	found := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".go") {
			t.Fatalf("cannot parse git grep output; expected a path, got:\n  %s\n"+
				"The filename field is unusable, so this test can prove nothing. "+
				"Check whether git is colorizing into a pipe (color.ui or color.grep set to `always`).", line)
		}
		found[line] = true
	}
	if len(found) == 0 {
		t.Fatal("guard matched no ResolveSessionFile callers at all; the detection pattern has gone stale and must be re-pointed")
	}

	for file := range found {
		if _, sanctioned := resolveSessionFileCallers[file]; !sanctioned {
			t.Errorf("%s calls ResolveSessionFile directly with an ID this guard cannot prove was validated.\n"+
				"Resolve through agent.OpenSessionStore(...).SessionFile(id) instead: it validates the ID "+
				"and rejects one that resolved outside the store. See the contract on agent.Agent.ResolveSessionFile.", file)
		}
	}
	for file, why := range resolveSessionFileCallers {
		if !found[file] {
			t.Errorf("%s is sanctioned to call ResolveSessionFile (%s) but no longer does; remove the entry.", file, why)
		}
	}
}
