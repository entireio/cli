package cli

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestAreHooksInstalledMatchesUninstallHooks pins the invariant that makes the
// probe usable as a removal gate: for every built-in agent, AreHooksInstalled is
// true exactly while UninstallHooks still has something of Entire's to strip.
//
// The two answers used to be written independently, and detection was the
// narrower of the pair — Claude Code probed the Stop hook alone while removal
// covered six hook types and a permissions rule. Every caller that skips removal
// on a false answer (`entire agent remove`, the `entire enable` deselect path)
// therefore left hooks on disk that go on invoking an Entire no longer
// configured there.
//
// This is the shape-agnostic half of that guard: it only installs and removes,
// so it keeps covering an agent whose config file, format, or hook set moves.
// The partial-config cases, where the two answers actually used to diverge, are
// pinned per agent in each agent package's own tests.
func TestAreHooksInstalledMatchesUninstallHooks(t *testing.T) {
	// Cannot use t.Parallel: every agent resolves its config path from the
	// working directory, and each subtest chdirs into its own repository.
	ctx := context.Background()
	covered := 0
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			t.Errorf("agent.Get(%s): %v", name, err)
			continue
		}
		if to, ok := ag.(agent.TestOnly); ok && to.IsTestOnly() {
			continue
		}
		hooks, ok := agent.AsHookSupport(ag)
		if !ok {
			continue
		}
		covered++
		t.Run(string(name), func(t *testing.T) {
			repoRoot := t.TempDir()
			testutil.InitRepo(t, repoRoot)
			t.Chdir(repoRoot)

			if _, err := hooks.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
			if !hooks.AreHooksInstalled(ctx) {
				t.Fatal("AreHooksInstalled() = false right after InstallHooks()")
			}

			if err := hooks.UninstallHooks(ctx); err != nil {
				t.Fatalf("UninstallHooks() error = %v", err)
			}
			if hooks.AreHooksInstalled(ctx) {
				t.Error("AreHooksInstalled() = true after UninstallHooks(), so the probe " +
					"reports an installation that removal cannot remove")
			}
			if leftover := findEntireHookCommand(t, repoRoot); leftover != "" {
				t.Errorf("UninstallHooks() left an Entire hook command in %s", leftover)
			}

			// The uninstall paths call removal without knowing whether an earlier
			// run already finished, so a second pass must be a quiet no-op.
			if err := hooks.UninstallHooks(ctx); err != nil {
				t.Fatalf("second UninstallHooks() error = %v", err)
			}
			if hooks.AreHooksInstalled(ctx) {
				t.Error("AreHooksInstalled() = true after a second UninstallHooks()")
			}
		})
	}

	// A registry that stopped yielding hook-capable agents would leave every
	// assertion above unreached and this test green while guarding nothing.
	if covered == 0 {
		t.Fatal("no hook-capable agents were exercised; the registry lookup above is broken")
	}
}

// findEntireHookCommand returns the first repo-relative path whose contents name
// an Entire hook command, or "" when none do. It walks the whole tree rather
// than a per-agent config path so that an agent moving its config cannot quietly
// drop out of the check.
func findEntireHookCommand(t *testing.T, repoRoot string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "entire hooks ") {
			relative, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				relative = path
			}
			found = relative
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	return found
}
