//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// writeExternalHooksSettings overwrites .entire/settings.json with the
// external git hooks backend pointed at the given external_dir.
// Replaces (not merges) because InitEntire's writer used the wrong shape
// for our discriminated union — full overwrite is the cleanest path.
func writeExternalHooksSettings(t *testing.T, env *TestEnv, externalDir string) {
	t.Helper()
	settings := map[string]any{
		"enabled":   true,
		"local_dev": true,
		"strategy_options": map[string]any{
			"filtered_fetches": true,
		},
		"git_hooks": map[string]any{
			"backend":      "external",
			"external_dir": externalDir,
		},
	}
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	settingsPath := filepath.Join(env.RepoDir, ".entire", "settings.json")
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// snapshotFiles returns a flat map of relpath→contents for every regular
// file under root. Used for byte-identical comparison after `entire enable`
// runs in external mode — the contract says nothing outside marker reads
// should change on disk.
func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func mapsEqual(a, b map[string]string) (string, bool) {
	for k, v := range a {
		bv, ok := b[k]
		if !ok {
			return "missing key " + k, false
		}
		if bv != v {
			return "content changed for " + k, false
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			return "unexpected new key " + k, false
		}
	}
	return "", true
}

// TestEnable_ExternalBackend_HuskyShape_EndToEnd is the end-to-end tracer for
// the external git hooks backend. It exercises the full `entire enable` path
// in a Husky-shaped repository (.husky/_/* stubs + .husky/<hook> user scripts)
// and asserts:
//  1. exit code 0
//  2. stdout contains the variant-3 hint line
//  3. .git/hooks/ is byte-identical before and after (no install)
//  4. .husky/ is byte-identical before and after (no writes to user dir)
//
// This test is the safety net for the design contract: external mode must
// be detection-only. If any cycle's GREEN implementation accidentally writes
// to .git/hooks/ or .husky/, this test catches it.
func TestEnable_ExternalBackend_HuskyShape_EndToEnd(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	// Husky shape: stubs in .husky/_/, user scripts in .husky/<hook>.
	// Each user script carries the Entire marker and a dispatch call so
	// IsGitHookInstalled detects them.
	huskyDir := filepath.Join(env.RepoDir, ".husky")
	huskyStubsDir := filepath.Join(huskyDir, "_")
	if err := os.MkdirAll(huskyStubsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	managedHooks := []string{"prepare-commit-msg", "commit-msg", "post-commit", "post-rewrite", "pre-push"}
	for _, h := range managedHooks {
		// .husky/_/<hook>: Husky's stub (we don't write or touch these)
		stubContent := "#!/bin/sh\n# managed by husky — DO NOT EDIT\n" + h + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(huskyStubsDir, h), []byte(stubContent), 0o755); err != nil {
			t.Fatal(err)
		}
		// .husky/<hook>: user-owned script containing the Entire marker
		userContent := "#!/bin/sh\n# Entire CLI hooks\nentire hooks git " + h + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(huskyDir, h), []byte(userContent), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Configure external backend pointed at .husky/
	writeExternalHooksSettings(t, env, ".husky")

	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")
	gitHooksBefore := snapshotFiles(t, hooksDir)
	huskyBefore := snapshotFiles(t, huskyDir)

	output, err := env.RunCLIWithError("enable")
	if err != nil {
		t.Fatalf("enable failed: %v\noutput:\n%s", err, output)
	}

	// Variant-3 success hint
	wantHint := "Git hooks: external (.husky)"
	if !strings.Contains(output, wantHint) {
		t.Errorf("expected output to contain %q\nfull output:\n%s", wantHint, output)
	}

	// .git/hooks/ unchanged (entire never wrote here)
	gitHooksAfter := snapshotFiles(t, hooksDir)
	if msg, ok := mapsEqual(gitHooksBefore, gitHooksAfter); !ok {
		t.Errorf(".git/hooks/ changed in external mode: %s\nbefore: %d files\nafter: %d files",
			msg, len(gitHooksBefore), len(gitHooksAfter))
	}

	// .husky/ unchanged (user-owned directory; we promised not to touch it)
	huskyAfter := snapshotFiles(t, huskyDir)
	if msg, ok := mapsEqual(huskyBefore, huskyAfter); !ok {
		t.Errorf(".husky/ changed in external mode: %s\nbefore: %d files\nafter: %d files",
			msg, len(huskyBefore), len(huskyAfter))
	}
}

// TestAgentAdd_ExternalBackend_MissingDir_AbortsBeforeAgentFiles guards
// against a bug where `entire agent add` (unlike `entire enable`) wrote
// agent-side files before checking the external-hooks precondition,
// leaving a half-enabled state that users had to unwind by hand. The
// precondition now runs first, so no agent files should exist after the
// aborted call.
func TestAgentAdd_ExternalBackend_MissingDir_AbortsBeforeAgentFiles(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	// external_dir points at a directory that does not exist on disk.
	writeExternalHooksSettings(t, env, ".husky")

	// Snapshot the repo root before the aborted call so we can prove no
	// agent-side files (.claude/, .entire/, etc.) were written.
	before := snapshotFiles(t, env.RepoDir)

	output, err := env.RunCLIWithError("agent", "add", "claude-code")
	if err == nil {
		t.Fatalf("agent add should fail when external_dir is missing\noutput:\n%s", output)
	}
	if !strings.Contains(output, "Required setup for external git hooks") {
		t.Errorf("output should carry the external-hooks help block\noutput:\n%s", output)
	}

	after := snapshotFiles(t, env.RepoDir)
	// Any new file that appeared under the repo root is a leaked write.
	for k := range after {
		if _, existed := before[k]; !existed {
			t.Errorf("agent add wrote %q after failing external-hooks gate; expected no partial state", k)
		}
	}
}

// path: external + dir absent → exit non-zero + full instructional message
// printed. We don't assert the entire 30+ line block, just the key markers
// that prove it came from FormatExternalDirMissingHelp.
func TestEnable_ExternalBackend_MissingDir_AbortsWithHelp(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	// Configure external backend pointing to a directory that does NOT exist
	writeExternalHooksSettings(t, env, ".husky")

	output, err := env.RunCLIWithError("enable")
	if err == nil {
		t.Fatalf("enable should fail when external_dir is missing\noutput:\n%s", output)
	}

	// Output must contain the instructional message key phrases
	mustContain := []string{
		`.husky`,
		"Required setup for external git hooks",
		"# Entire CLI hooks",
		"prepare-commit-msg",
		"commit-msg",
		"post-commit",
		"post-rewrite",
		"pre-push",
	}
	for _, s := range mustContain {
		if !strings.Contains(output, s) {
			t.Errorf("output missing %q\nfull output:\n%s", s, output)
		}
	}
}
