//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// writeExternalHooksSettings overwrites .entire/settings.json with the
// external git hooks backend (manager="scripts") pointed at the given
// external_path. Replaces (not merges) because InitEntire's writer used
// the wrong shape for our discriminated union — full overwrite is the
// cleanest path.
func writeExternalHooksSettings(t *testing.T, env *TestEnv, externalPath string) {
	t.Helper()
	settings := map[string]any{
		"enabled":   true,
		"local_dev": true,
		"strategy_options": map[string]any{
			"filtered_fetches": true,
		},
		"git_hooks": map[string]any{
			"backend":       "external",
			"manager":       "scripts",
			"external_path": externalPath,
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
	testBinary := getTestBinary()
	for _, h := range managedHooks {
		// .husky/_/<hook>: Husky's stub (we don't write or touch these)
		stubContent := "#!/bin/sh\n# managed by husky — DO NOT EDIT\n" + h + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(huskyStubsDir, h), []byte(stubContent), 0o755); err != nil {
			t.Fatal(err)
		}
		// .husky/<hook>: user-owned script containing the Entire marker.
		// Invoke the test binary directly rather than relying on PATH; the
		// child shell run by `git commit` inherits the same env we set on
		// RunCLIWithError, but does not have `entire` on PATH.
		//
		// Match production semantics: every hook except pre-push swallows
		// its exit code, but pre-push must propagate so OPF can abort push
		// on unredacted content.
		dispatch := testBinary + " hooks git " + h + " \"$@\""
		if h != "pre-push" {
			dispatch += " || true"
		}
		userContent := "#!/bin/sh\n# Entire CLI hooks\n" + dispatch + "\n"
		if err := os.WriteFile(filepath.Join(huskyDir, h), []byte(userContent), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Configure external backend pointed at .husky/
	writeExternalHooksSettings(t, env, ".husky")

	// NewRepoWithCommit pins core.hooksPath at .git/hooks for direct-mode
	// tests; point it at .husky/ instead so git actually routes hook events
	// through the user-owned scripts we just created (real Husky sets this
	// during `husky install`). Rebaseline the config guard so cleanup
	// tolerates the intentional change.
	hooksPathCmd := execx.NonInteractive(context.Background(), "git", "config", "--local", "core.hooksPath", ".husky")
	hooksPathCmd.Dir = env.RepoDir
	hooksPathCmd.Env = env.cliEnv()
	if out, err := hooksPathCmd.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath failed: %v\noutput:\n%s", err, string(out))
	}
	env.setGitConfigBaseline()

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

	// Detection-only contract is only useful if git actually routes hook
	// events through the user's .husky/ scripts. Run a real commit and
	// assert (1) the commit succeeds and (2) it produces the same
	// Entire-side side effects a direct-mode commit would, proving the
	// marker-based dispatch reached `entire hooks git ...`.
	if err := os.WriteFile(filepath.Join(env.RepoDir, "trigger.txt"), []byte("husky trigger\n"), 0o644); err != nil {
		t.Fatalf("write trigger file: %v", err)
	}
	env.GitAdd("trigger.txt")

	// Take a snapshot of the entire log dir before the commit so we can
	// detect writes even if the directory pre-exists from `entire enable`.
	logsDir := filepath.Join(env.RepoDir, ".entire", "logs")
	logsBefore := snapshotFiles(t, logsDir)

	commitCmd := execx.NonInteractive(context.Background(), "git", "commit", "-m", "trigger husky hooks", "--no-gpg-sign")
	commitCmd.Dir = env.RepoDir
	commitCmd.Env = env.cliEnv()
	if commitOut, cErr := commitCmd.CombinedOutput(); cErr != nil {
		t.Fatalf("real git commit through husky hooks failed: %v\noutput:\n%s", cErr, string(commitOut))
	}

	// entire's log dir should have grown (existing files updated or new ones
	// added) once the husky-dispatched hooks called into `entire hooks git`.
	// Any observable change to the log tree proves the marker-based dispatch
	// reached the entire binary.
	logsAfter := snapshotFiles(t, logsDir)
	if _, unchanged := mapsEqual(logsBefore, logsAfter); unchanged {
		t.Errorf(".entire/logs/ unchanged after husky-dispatched commit (before=%d files, after=%d); expected entire hook to have been invoked", len(logsBefore), len(logsAfter))
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

	// external_path points at a directory that does not exist on disk.
	writeExternalHooksSettings(t, env, ".husky")

	// Snapshot the repo root before the aborted call so we can prove no
	// agent-side files (.claude/, .entire/, etc.) were written.
	before := snapshotFiles(t, env.RepoDir)

	output, err := env.RunCLIWithError("agent", "add", "claude-code")
	if err == nil {
		t.Fatalf("agent add should fail when external_path is missing\noutput:\n%s", output)
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

// path: external + external_path absent → exit non-zero + full instructional
// message printed. We don't assert the entire 30+ line block, just the key
// markers that prove it came from FormatExternalDirMissingHelp.
func TestEnable_ExternalBackend_MissingDir_AbortsWithHelp(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	// Configure external backend pointing to a directory that does NOT exist
	writeExternalHooksSettings(t, env, ".husky")

	output, err := env.RunCLIWithError("enable")
	if err == nil {
		t.Fatalf("enable should fail when external_path is missing\noutput:\n%s", output)
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

// writeLefthookManagerSettings overwrites .entire/settings.json with the
// external backend in lefthook manager mode, pointed at the exact
// lefthook.yml config file this test writes.
func writeLefthookManagerSettings(t *testing.T, env *TestEnv) {
	t.Helper()
	settings := map[string]any{
		"enabled":   true,
		"local_dev": true,
		"strategy_options": map[string]any{
			"filtered_fetches": true,
		},
		"git_hooks": map[string]any{
			"backend":       "external",
			"manager":       "lefthook",
			"external_path": "lefthook.yml",
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

// lefthookAllHooksConfig wires every managed hook to "./scripts/entire-dev",
// matching the cmdPrefix HookCmdPrefix derives for local_dev=true (the mode
// writeLefthookManagerSettings configures).
const lefthookAllHooksConfig = `prepare-commit-msg:
  commands:
    entire:
      run: ./scripts/entire-dev hooks git prepare-commit-msg {1} {2}
commit-msg:
  commands:
    entire:
      run: ./scripts/entire-dev hooks git commit-msg {1}
post-commit:
  commands:
    entire:
      run: ./scripts/entire-dev hooks git post-commit
post-rewrite:
  commands:
    entire:
      run: ./scripts/entire-dev hooks git post-rewrite {1}
pre-push:
  commands:
    entire:
      run: ./scripts/entire-dev hooks git pre-push {1}
`

// TestEnable_ExternalBackend_Lefthook_AllHooksWired verifies that with the
// lefthook manager configured and a lefthook.yml wiring all 5 managed hooks,
// `entire enable` succeeds and reports the lefthook status line — proving
// config-driven detection works end to end through the real binary.
func TestEnable_ExternalBackend_Lefthook_AllHooksWired(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	writeLefthookManagerSettings(t, env)
	if err := os.WriteFile(filepath.Join(env.RepoDir, "lefthook.yml"), []byte(lefthookAllHooksConfig), 0o644); err != nil {
		t.Fatalf("write lefthook.yml: %v", err)
	}

	output, err := env.RunCLIWithError("enable")
	if err != nil {
		t.Fatalf("enable failed: %v\noutput:\n%s", err, output)
	}
	if !strings.Contains(output, "lefthook") {
		t.Errorf("expected output to name the lefthook manager\nfull output:\n%s", output)
	}

	// doctor should report the wiring as healthy.
	doctorOut, dErr := env.RunCLIWithError("doctor")
	if dErr != nil {
		t.Fatalf("doctor failed unexpectedly: %v\noutput:\n%s", dErr, doctorOut)
	}
	if !strings.Contains(doctorOut, "✓ External git hooks") {
		t.Errorf("doctor should report external git hooks healthy\noutput:\n%s", doctorOut)
	}
}

// TestEnable_ExternalBackend_Lefthook_MissingHookAborts verifies that a
// lefthook.yml missing one managed hook (pre-push) makes `entire enable`
// abort with the instructional help naming the missing hook.
func TestEnable_ExternalBackend_Lefthook_MissingHookAborts(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	writeLefthookManagerSettings(t, env)
	partial := strings.Replace(lefthookAllHooksConfig,
		"pre-push:\n  commands:\n    entire:\n      run: ./scripts/entire-dev hooks git pre-push {1}\n", "", 1)
	if err := os.WriteFile(filepath.Join(env.RepoDir, "lefthook.yml"), []byte(partial), 0o644); err != nil {
		t.Fatalf("write lefthook.yml: %v", err)
	}

	output, err := env.RunCLIWithError("enable")
	if err == nil {
		t.Fatalf("enable should abort when a lefthook hook is not wired\noutput:\n%s", output)
	}
	for _, s := range []string{"lefthook", "pre-push", "lefthook install"} {
		if !strings.Contains(output, s) {
			t.Errorf("output missing %q\nfull output:\n%s", s, output)
		}
	}
}
