package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// AssertCommittedDogfoodFile checks that a generated agent config this repo
// commits for its own use (e.g. .pi/extensions/entire/index.ts) still matches
// what InstallHooks would write today, given repoRelPath and the rendered
// content.
//
// These files are generated but tracked, so they only get refreshed when someone
// re-runs an install — and nothing failed when they didn't. They drifted that way
// once already: the committed copies kept a stale render long after the template
// moved on, and separately pointed at a launcher script inside the working tree
// that was later deleted, leaving hooks invoking a path that no longer existed.
// Runtime drift detection (CheckHookConfig, surfaced by `entire status` and
// `entire doctor`) only helps someone who runs those commands; this fails in CI
// instead.
//
// Fix a failure by re-rendering the committed file from the template — do not
// edit it by hand, and do not relax this check.
func AssertCommittedDogfoodFile(t *testing.T, repoRelPath, rendered string) {
	t.Helper()

	root := repoRoot(t)
	path := filepath.Join(root, repoRelPath)
	data, err := os.ReadFile(path) //nolint:gosec // path derived from the module root
	if err != nil {
		t.Fatalf("failed to read committed %s: %v", repoRelPath, err)
	}
	if string(data) != rendered {
		t.Errorf("committed %s is stale: it does not match what InstallHooks writes today.\n"+
			"Re-render it from the embedded template (substituting the entire command) and commit the result.", repoRelPath)
	}
}

// AssertCommittedDogfoodConfigStable is the equivalent of
// AssertCommittedDogfoodFile for agents whose committed config is JSON that
// InstallHooks edits in place rather than a whole generated file.
//
// It copies the committed config into an isolated temp repo, runs a non-force
// install there, and asserts the install reported nothing to do AND changed no
// bytes. That is the strongest statement available without re-deriving each
// agent's config format: if the committed file already is what InstallHooks
// writes, an install is a no-op.
//
// install receives the temp directory that the copied config sits under and must
// perform a non-force InstallHooks rooted there, returning the hook count.
//
// Fix a failure by re-running the install against the repo and committing the
// result — do not hand-edit the committed config.
func AssertCommittedDogfoodConfigStable(t *testing.T, repoRelPath string, install func(t *testing.T, dir string) (int, error)) {
	t.Helper()

	committed := filepath.Join(repoRoot(t), repoRelPath)
	before, err := os.ReadFile(committed) //nolint:gosec // path derived from the module root
	if err != nil {
		t.Fatalf("failed to read committed %s: %v", repoRelPath, err)
	}

	dir := t.TempDir()
	dst := filepath.Join(dir, repoRelPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, before, 0o600); err != nil { //nolint:gosec // dst is filepath.Join of a t.TempDir() and a test-supplied relative literal
		t.Fatal(err)
	}

	count, err := install(t, dir)
	if err != nil {
		t.Fatalf("install against a copy of %s failed: %v", repoRelPath, err)
	}

	after, readErr := os.ReadFile(dst) //nolint:gosec // test-controlled path
	if readErr != nil {
		t.Fatalf("failed to read %s after install: %v", repoRelPath, readErr)
	}
	if count != 0 || string(after) != string(before) {
		t.Errorf("committed %s is stale: a plain install reported %d hooks and rewrote the file.\n"+
			"Re-run the install against the repo and commit the result.\n--- committed ---\n%s\n--- install writes ---\n%s",
			repoRelPath, count, before, after)
	}
}

// repoRoot returns the module root, located by walking up from this source
// file's own path.
//
// Deliberately not derived from the working directory: callers live in packages
// whose tests t.Chdir into temp repos, so a CWD-relative walk would resolve
// somewhere else entirely (and os.Getwd is forbidden here for that reason).
func repoRoot(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve this file's path via runtime.Caller")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}
