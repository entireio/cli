package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// canonicalizeRepoRefSites is every non-test file that turns a typed repo ref
// into its canonical /et/... or /gh/... path, with what it does there.
//
// An entire.com web URL is accepted as a second spelling of a ref only at the
// command's typed argument, once, through newTypedRef (canonicalizeRepoRef
// underneath). The strict parsers and the parsers of server text never accept
// the URL form. A command that parses a user-typed ref without going through
// newTypedRef therefore refuses a pasted URL that its siblings accept, and
// one that canonicalizes somewhere deeper would accept a URL from a server
// answer or a git remote. This ledger makes adding either a decision.
//
// The guard is file-granular: it pins only that each listed file calls the
// helper, not that every command in it does. Per-command behaviour is pinned
// by the per-command URL tests (repo_ref_url_test.go,
// repo_ref_url_sites_test.go, and the grant, invite, mirror and
// service-account tests).
//
// The needles cover the two call shapes: newTypedRef( itself, and the
// .typedRef( helpers on grantTarget and inviteScope, which wrap it so that
// invite_scope.go never names newTypedRef directly.
var canonicalizeRepoRefSites = map[string]string{
	"cmd/entire/cli/explain_repo.go":          "canonicalizes the --repo value before the forge/owner/repo split",
	"cmd/entire/cli/grant.go":                 "grantTarget.typedRef canonicalizes the repo arg of every grant leaf (and skips non-repo targets)",
	"cmd/entire/cli/invite_scope.go":          "inviteScope.typedRef canonicalizes the repo arg of the invite leaves (and skips non-repo scopes)",
	"cmd/entire/cli/repo.go":                  "canonicalizes the ref argument of the repo view/ref-resolving commands",
	"cmd/entire/cli/repo_clone.go":            "defines newTypedRef and canonicalizes the `repo clone` argument",
	"cmd/entire/cli/repo_default_branch.go":   "canonicalizes the ref argument of `repo default-branch`",
	"cmd/entire/cli/repo_delete.go":           "canonicalizes the ref argument of `repo delete`",
	"cmd/entire/cli/repo_mirror.go":           "canonicalizes the ref argument of `repo mirror add`",
	"cmd/entire/cli/repo_mirror_detach.go":    "canonicalizes the repo and --into arguments of `repo mirror detach`",
	"cmd/entire/cli/repo_mirror_remove.go":    "canonicalizes the ref argument of `repo mirror remove`",
	"cmd/entire/cli/repo_mirror_sync.go":      "canonicalizes the ref argument of `repo mirror resync`",
	"cmd/entire/cli/repo_protection.go":       "canonicalizes the ref argument of the `repo protection` leaves",
	"cmd/entire/cli/repo_remote.go":           "canonicalizes the ref argument of `repo remote`",
	"cmd/entire/cli/service_account_grant.go": "canonicalizes the repo argument of the service-account grant leaves",
}

// TestCanonicalizeRepoRefSitesAreAudited fails when a file starts or stops
// canonicalizing a typed ref without the ledger above saying so.
func TestCanonicalizeRepoRefSitesAreAudited(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	found := map[string]struct{}{}
	for _, needle := range []string{"newTypedRef(", ".typedRef("} {
		out := testutil.GitGrepGuard(t, repoRoot, "-l", "--fixed-strings", "--", needle,
			"--", ":(glob)**/*.go", ":(exclude,glob)**/*_test.go")
		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			if !strings.HasSuffix(line, ".go") {
				t.Fatalf("cannot parse git grep -l output; expected a .go path, got:\n  %s", line)
			}
			found[line] = struct{}{}
		}
	}
	if len(found) == 0 {
		t.Fatal("guard matched no call sites at all; the detection pattern has gone stale")
	}

	for file := range found {
		if _, audited := canonicalizeRepoRefSites[file]; audited {
			continue
		}
		t.Errorf("%s canonicalizes a typed repo ref (newTypedRef / .typedRef) but is not in "+
			"canonicalizeRepoRefSites.\nAdd an entry saying what it canonicalizes. The URL "+
			"spelling is accepted only at a command's typed argument, never in a strict or "+
			"server-text parser.", file)
	}
	for file := range canonicalizeRepoRefSites {
		if _, still := found[file]; !still {
			t.Errorf("%s is in canonicalizeRepoRefSites but no longer calls newTypedRef( or "+
				".typedRef(; remove the entry", file)
		}
	}
}
