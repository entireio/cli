package osroot_test

import (
	"os/exec"
	"strings"
	"testing"
)

// rootOpeners are the two ways a *os.Root comes into existence in this
// codebase. os.OpenRoot is Go's; osroot.Shared is the memoizing registry every
// long-lived anchor goes through.
var rootOpeners = []string{"os.OpenRoot(", "osroot.Shared("}

// allowedRootBases lists every non-test file permitted to open a root, with the
// reason its base is a trusted path.
//
// A root is only as trustworthy as the directory it is opened on. The failure
// this guards against is subtle because it looks like the fix: opening a root at
// filepath.Dir of the file you are about to read puts every component the
// caller resolved ABOVE the root, so the containment covers only the final name
// and enforces nothing that the join did not already decide. Anchoring instead
// on a directory a resolver produced — the worktree root, the git common dir,
// $ENTIRE_CONFIG_DIR — makes the components in between names inside the root,
// which is what os.Root can actually refuse.
//
// Adding an entry here is a deliberate act. Prefer an existing anchor
// (entiredir, gitdir, worktreedir, userdirs, agent.SessionStore) over a new
// root; those exist so a call site does not have to decide what its base is.
var allowedRootBases = map[string]string{
	// The anchors themselves. Each opens exactly one directory, resolved
	// independently of anything it is later asked to read.
	"cmd/entire/cli/osroot/osroot.go":            "the registry",
	"cmd/entire/cli/entiredir/entiredir.go":      "worktree root + .entire",
	"cmd/entire/cli/gitdir/gitdir.go":            "git rev-parse --git-common-dir",
	"cmd/entire/cli/worktreedir/worktreedir.go":  "paths.WorktreeRoot",
	"internal/entireclient/userdirs/userdirs.go": "$ENTIRE_CONFIG_DIR / $XDG_CACHE_HOME",

	// Trees with their own resolver, anchored at the boundary between what
	// Entire owns and what it does not.
	"cmd/entire/cli/agent/session_store.go":      "the agent's own GetSessionDir",
	"cmd/entire/cli/plugin_store.go":             "pluginParentDir()",
	"cmd/entire/cli/plugin_index.go":             "the per-index cache dir, opened at the clone it contains",
	"cmd/entire/cli/plugin_install_remote.go":    "a staging dir this process just created",
	"cmd/entire/cli/plugin_fetch.go":             "the staging dir its caller created",
	"cmd/entire/cli/utils.go":                    "one of worktree root / home / temp, chosen by containment",
	"internal/entireclient/contexts/contexts.go": "the caller's config dir, not the contexts file's parent",
	"internal/entireclient/discovery/cache.go":   "the caller's cache dir, not the cache file's parent",

	// The two deliberate exceptions, both on a path the CALLER named, where
	// the file's parent IS the caller's choice and no other base exists. Each
	// carries the reasoning at the call site.
	"internal/entireclient/tokenstore/file.go": "$ENTIRE_TOKEN_STORE_PATH's own directory",
	"cmd/entire/cli/settings/settings.go":      "an explicitly-named settings file's own directory",
}

// TestRootBasesAreTrusted fails the build when a new file opens an os.Root
// without being listed above.
//
// It is a source-level guard rather than a comment because the wrong version of
// this code reads as correct: `osroot.Shared(filepath.Dir(abs))` looks like
// containment and compiles to almost none. Three call sites shipped that shape
// before anyone noticed, and one of them (settings.clonePreferencesRoot) had a
// containment CHECK that could never fire because the base was derived from the
// path being checked.
func TestRootBasesAreTrusted(t *testing.T) {
	t.Parallel()

	repoRoot, err := exec.Command("git", "rev-parse", "--show-toplevel").Output() //nolint:noctx // guard test, no cancellation needed
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
	}

	var checked int
	for _, opener := range rootOpeners {
		grep := exec.Command("git", "grep", "-n", "--fixed-strings", "--", opener, "--", "cmd", "internal") //nolint:noctx // guard test, no cancellation needed
		grep.Dir = strings.TrimSpace(string(repoRoot))
		out, grepErr := grep.Output()
		if grepErr != nil {
			t.Fatalf("git grep for %q found nothing, which cannot be right: %v", opener, grepErr)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			file, rest, ok := strings.Cut(line, ":")
			if !ok || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
				continue
			}
			// The rule is about opening a root, not about naming one: doc
			// comments quote both spellings freely.
			if strings.Contains(rest, "//") && !strings.Contains(strings.SplitN(rest, "//", 2)[0], opener) {
				continue
			}
			checked++
			if _, allowed := allowedRootBases[file]; allowed {
				continue
			}
			t.Errorf("%s opens an os.Root but is not in allowedRootBases:\n  %s\n"+
				"An os.Root's base directory must be a trusted path — one a resolver "+
				"produced — never filepath.Dir of the file being opened and never a "+
				"path that arrived as data. Prefer an existing anchor (entiredir, "+
				"gitdir, worktreedir, userdirs, agent.SessionStore); if this really "+
				"needs its own root, add it to allowedRootBases with the reason its "+
				"base is trusted. See \"The Root Anchors\" in CLAUDE.md.", file, line)
		}
	}

	// If a refactor moves every open behind a helper this pattern no longer
	// recognises, the guard would silently pass forever.
	if checked == 0 {
		t.Error("guard matched no root opens at all; the detection pattern has gone " +
			"stale and must be re-pointed (rootOpeners)")
	}

	for file := range allowedRootBases {
		if !fileStillOpensARoot(t, strings.TrimSpace(string(repoRoot)), file) {
			t.Errorf("%s is in allowedRootBases but no longer opens a root; remove the entry", file)
		}
	}
}

// fileStillOpensARoot keeps allowedRootBases from accumulating entries for code
// that has moved on. An allowlist nobody prunes is how an exemption outlives its
// reason.
func fileStillOpensARoot(t *testing.T, repoRoot, file string) bool {
	t.Helper()
	for _, opener := range rootOpeners {
		grep := exec.Command("git", "grep", "-q", "--fixed-strings", "--", opener, "--", file) //nolint:noctx // guard test, no cancellation needed
		grep.Dir = repoRoot
		if grep.Run() == nil {
			return true
		}
	}
	return false
}
