package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// userDirConsumers is every file that calls userdirs.Config() or
// userdirs.Cache(), with the reason a bad override cannot cause damage there.
//
// Those two return a plain string and cannot report a rejected override, so
// safety is the CALLER's, and it has to arrive before the caller creates a
// directory, takes a lock, or writes a file. There are only two safe shapes:
// hand the value straight to a consumer that checks (contexts, discovery, the
// userdirs roots), or call the *Checked resolver and handle the error.
//
// Converting a consumer to the *Checked resolver removes it from this ledger
// entirely, because it is then safe by construction rather than by argument —
// plugin_index left the list that way. The list is meant to shrink.
//
// A ledger rather than a doc comment because the doc comment was tried twice
// and was wrong both times within a commit or two. The token store slipped past
// the first enumeration and put bearer tokens at ./<value>/tokens.json;
// plugin_index slipped past the second and put an index clone and its lock file
// in the working directory. Prose cannot fail when someone adds a caller.
var userDirConsumers = map[string]string{
	"cmd/entire/cli/auth/cell_data_api.go":        "passes both directories to clusterdiscovery, which reaches contexts and discovery; neither creates before checking",
	"cmd/entire/cli/auth/context_store.go":        "passes the config dir to contexts.Load/Modify, which check before EnsurePrivateDir",
	"cmd/entire/cli/auth/contexts.go":             "passes the config dir to contexts.Modify",
	"cmd/entire/cli/auth/control_plane.go":        "passes both to clusterdiscovery and contexts.Load",
	"cmd/entire/cli/auth/data_api.go":             "passes both to clusterdiscovery",
	"cmd/entire/cli/versioncheck/versioncheck.go": "only ever creates through userdirs.ConfigRoot, whose resolveUserRoot checks before creating",
	"cmd/git-remote-entire/main.go":               "passes both to clusterdiscovery, which reaches contexts and discovery",
	"internal/remotehelper/replicas/replicas.go":  "passes the cache dir to discovery.LoadCache/ModifyCache, which check before MkdirAll",
}

// TestUserDirConsumersAreAudited fails the build when a new caller of
// userdirs.Config() or userdirs.Cache() appears without an entry above, and
// when an entry outlives the call it describes.
//
// It cannot verify that the stated reason is TRUE — that is the reviewer's job,
// and writing the reason down is what gives them something to check. What it
// guarantees is that nobody adds a consumer without being asked the question.
func TestUserDirConsumersAreAudited(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	found := map[string]struct{}{}
	for _, needle := range []string{"userdirs.Config()", "userdirs.Cache()"} {
		out := testutil.GitGrepGuard(t, repoRoot, "-l", "--fixed-strings", "--", needle,
			"--", ":(glob)**/*.go", ":(exclude,glob)**/*_test.go")
		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			if !strings.HasSuffix(line, ".go") {
				t.Fatalf("cannot parse git grep -l output; expected a .go path, got:\n  %s", line)
			}
			// The resolver defines them; it is not a consumer of itself.
			if strings.HasSuffix(line, "internal/entireclient/userdirs/userdirs.go") {
				continue
			}
			// A mention inside a doc comment is not a call. contexts names
			// userdirs.Config() when explaining what its base is.
			if strings.HasSuffix(line, "internal/entireclient/contexts/contexts.go") {
				continue
			}
			found[line] = struct{}{}
		}
	}
	if len(found) == 0 {
		t.Fatal("guard matched no consumers at all; the detection pattern has gone stale")
	}

	for file := range found {
		if _, audited := userDirConsumers[file]; audited {
			continue
		}
		t.Errorf("%s calls userdirs.Config() or userdirs.Cache() but is not in userDirConsumers.\n"+
			"Those return a plain string and cannot report a rejected override, so this file is "+
			"responsible for learning about one BEFORE it creates a directory, takes a lock, or "+
			"writes. Either hand the value straight to a consumer that checks (contexts, "+
			"discovery, the userdirs roots), or call userdirs.ConfigDirChecked / CacheDirChecked "+
			"and handle the error — then add an entry saying which.", file)
	}
	for file := range userDirConsumers {
		if _, still := found[file]; !still {
			t.Errorf("%s is in userDirConsumers but no longer calls userdirs.Config() or "+
				"userdirs.Cache(); remove the entry", file)
		}
	}
}
