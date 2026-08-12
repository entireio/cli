package paths_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestNoDirectEntireJoins is a call-site guard for invisible routing: every
// runtime-data path under .entire must resolve through paths.AbsPath, which
// reroutes globally tracked repos to the git common dir. A direct
// filepath.Join(<base dir>, ".entire"...) (or with the paths.Entire*
// constants) bypasses that routing and silently writes into the worktree of
// a repo that must stay invisible.
//
// The test scans non-test production sources under cmd/ and internal/ and
// fails on any new bypass. Deliberate, audited call sites are allowlisted
// below with their rationale. Joins whose FIRST argument is itself an
// Entire* constant only build a relative subpath (which is then fed to
// AbsPath) and are not flagged. (.entire/worktrees — trail checkout
// worktrees — is deliberately worktree-resolved and documented on
// runtimeDataPrefixes; it is referenced via its own constant, not these
// patterns.)
func TestNoDirectEntireJoins(t *testing.T) {
	t.Parallel()

	// Audited-deliberate bypasses, by path suffix (slash-separated).
	allowlist := map[string]string{
		// The routing implementation itself: the settings-file Lstat is the
		// discriminator that DECIDES whether rerouting applies.
		"cmd/entire/cli/paths/invisible.go": "routing discriminator",
		// Runner definitions are repo-level configuration (like settings and
		// redactor packs), only meaningful with repo-level setup.
		"cmd/entire/cli/runner_prompt.go": "repo-level runner config, config-class not runtime",
		// Doctor bundle collects the repo-level settings files for support
		// bundles; a globally tracked repo has none by definition.
		"cmd/entire/cli/doctor_bundle.go": "collects repo-level settings files",
		// The takeover migration's DESTINATION is deliberately the literal
		// worktree .entire dir, in both of its states: on a fresh enable it
		// runs before .entire/settings.json exists, when AbsPath still routes
		// to the git common dir — resolving through AbsPath would move files
		// onto themselves; on a takeover recovery (enable/disable in a repo
		// whose settings file already exists) routing already points at the
		// worktree, and the destination must be that same literal dir
		// regardless. The join is routing-independent by design.
		"cmd/entire/cli/setup_global.go": "migration destination, routing-independent by design",
		// Legacy no-repo fallbacks, guarded by paths.IsUnroutableRuntimePath
		// so a tier-owned repo can never reach them.
		"cmd/entire/cli/agent/pi/lifecycle.go":       "sentinel-guarded no-repo fallback",
		"cmd/entire/cli/agent/opencode/lifecycle.go": "sentinel-guarded no-repo fallback",
	}

	// filepath.Join(...) with no nested calls; args inspected afterwards.
	joinCall := regexp.MustCompile(`filepath\.Join\(([^()]*)\)`)
	entireToken := regexp.MustCompile(`"\.entire|\bEntire(?:Dir|TmpDir|MetadataDir|LogsDir)\b`)
	entireConstFirst := regexp.MustCompile(`^\s*(?:paths\.)?Entire(?:Dir|TmpDir|MetadataDir|LogsDir)\b`)

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))

	var violations []string
	for _, scanRoot := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, scanRoot), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Test infrastructure builds fixtures in throwaway repos.
				switch d.Name() {
				case "testutil", "benchutil", "integration_test", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, m := range joinCall.FindAllStringSubmatch(string(data), -1) {
				args := m[1]
				firstArg, rest, found := strings.Cut(args, ",")
				if !found {
					continue // single-arg join cannot combine a base with .entire
				}
				if !entireToken.MatchString(rest) {
					continue
				}
				if entireConstFirst.MatchString(firstArg) {
					continue // builds a relative .entire subpath, not a resolved location
				}
				if _, allowed := allowlist[rel]; allowed {
					continue
				}
				violations = append(violations, rel+": "+strings.TrimSpace(m[0]))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", scanRoot, err)
		}
	}

	if len(violations) > 0 {
		t.Errorf("found filepath.Join calls that bypass paths.AbsPath for .entire runtime paths.\n"+
			"Resolve runtime paths through paths.AbsPath (invisible routing), or add an\n"+
			"audited allowlist entry in this test with a rationale:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
