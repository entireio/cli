//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// E-group: OPF pre-push rewrite over a real remote via the real git-invoked
// pre-push hook. OPF is git-branch only (the git-refs pre-push path descopes
// it), so every test here stays on the default git-branch backend.
//
// The unit tests in strategy/manual_commit_opf_rewrite_test.go cover the
// rewrite plumbing in-process with a fake runtime. These tests cover what the
// unit tests structurally cannot: OPF enabled via committed settings, resolved
// and executed inside a spawned hook subprocess, redacting real committed
// checkpoints and pushing the Entire-OPF-Applied trailer to a real bare remote.
//
// The spawned hook constructs a real redact.shellOut from settings and execs
// the configured `command`, so these tests point that command at a fake `opf`
// script on disk (writeFakeOPFScript). The in-process ConfigurePrivacyFilterWithRuntime
// seam the unit tests use is unavailable across the process boundary.

// writeFakeOPFScript writes an executable stand-in for the `opf` binary and
// returns its path. The real binary reads the concatenated batch on stdin and
// emits {"detected_spans":[...]} on stdout; this fake reads and discards stdin,
// records each invocation to markerFile (so a test can prove the subprocess
// actually ran), and reports no detected spans. Reporting no spans is enough to
// exercise the whole pipeline: a successful (exit 0, parseable) run means the
// rewrite tags commits Entire-OPF-Applied and pushes them, whereas a missing or
// broken binary would trip the circuit breaker and abort the push with
// OPFRuntimeFailedError. Content-level redaction correctness is covered by the
// strategy unit tests.
func writeFakeOPFScript(t *testing.T, markerFile string) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	scriptPath := filepath.Join(dir, "fake-opf")
	// Consume all of stdin (avoid SIGPIPE on the writer), record the call, emit
	// an empty-but-valid typed-JSON result.
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"echo invoked >> " + shellQuote(markerFile) + "\n" +
		"printf '%s' '{\"detected_spans\":[]}'\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755)) //nolint:gosec // fake binary must be executable
	return scriptPath
}

// shellQuote single-quotes s for safe embedding in a /bin/sh script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// enableOPF turns on the OpenAI Privacy Filter in the repo's committed settings,
// pointing the runtime at the fake opf script. prompt_default=always so the
// non-interactive hook runs OPF without prompting.
func enableOPF(t *testing.T, env *TestEnv, opfCommand string) {
	t.Helper()
	env.PatchSettings(map[string]any{
		"redaction": map[string]any{
			"openai_privacy_filter": map[string]any{
				"enabled":        true,
				"categories":     map[string]any{"private_person": true},
				"command":        opfCommand,
				"prompt_default": "always",
			},
		},
	})
}

// remoteMetadataTipMessage returns the full commit message of the remote v1
// branch tip. Reads the bare repo directly to avoid testing production code
// with itself.
func remoteMetadataTipMessage(t *testing.T, bareDir string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "log", "-1", "--format=%B", "refs/heads/"+paths.MetadataBranchName)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "read remote v1 tip message")
	return string(out)
}

// remoteBranchTip returns the commit hash the given branch points at on the
// bare remote, or "" if the branch is absent. Reads the bare repo directly.
func remoteBranchTip(t *testing.T, bareDir, branch string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, _ := cmd.Output() // non-zero exit (missing ref) yields empty output
	return strings.TrimSpace(string(out))
}

// TestOPFPrePush_HappyPath_TrailerLandsOnRemote is E1: with OPF enabled in
// committed settings, a plain `git push` runs the real pre-push hook, which
// resolves OPF from settings, rewrites the unpushed v1 commits with the
// Entire-OPF-Applied trailer, and pushes them. The remote v1 tip carries the
// trailer, the fake opf binary was actually invoked, and the user branch lands.
func TestOPFPrePush_HappyPath_TrailerLandsOnRemote(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	marker := filepath.Join(t.TempDir(), "opf-invocations")
	enableOPF(t, env, writeFakeOPFScript(t, marker))

	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	require.NotEmpty(t, checkpointID, "should have a checkpoint after condensation")

	out, err := env.GitPushArgsWithHooks("origin", "HEAD")
	require.NoError(t, err, "user push with OPF enabled should succeed:\n%s", out)

	// The rewritten remote v1 tip carries the OPF-applied trailer.
	require.Contains(t, remoteMetadataTipMessage(t, bareDir), "Entire-OPF-Applied: true",
		"remote v1 tip should carry the Entire-OPF-Applied trailer after the OPF pre-push rewrite")

	// The fake opf binary was actually executed by the spawned hook (proves OPF
	// ran rather than being silently skipped).
	data, readErr := os.ReadFile(marker) //nolint:gosec // path built from test temp dir
	require.NoError(t, readErr, "opf marker file should exist (opf binary should have been invoked)")
	require.NotEmpty(t, strings.TrimSpace(string(data)), "opf binary should have recorded at least one invocation")

	// The checkpoint and the user branch both reached the remote.
	require.True(t, env.CheckpointExistsOnRemote(bareDir, checkpointID),
		"checkpoint should be on the remote after the OPF pre-push")
	require.True(t, env.BranchExistsOnRemote(bareDir, "feature/test-branch"),
		"user feature branch should land on the remote alongside the rewritten checkpoints")
}

// TestOPFPrePush_DivergedV1_AbortsUserPush is E2: when OPF is enabled and local
// v1 has diverged from the remote (a shared base, then both sides added a
// checkpoint), the rewrite refuses with V1DivergedError before running OPF, and
// that error propagates out of the hook so `git push` exits non-zero with the
// actionable message. The user branch does NOT land.
func TestOPFPrePush_DivergedV1_AbortsUserPush(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	marker := filepath.Join(t.TempDir(), "opf-invocations")
	// A missing binary would be fine here too — divergence is detected before
	// OPF runs — but a real script keeps the setup uniform.
	enableOPF(t, env, writeFakeOPFScript(t, marker))

	// Shared base checkpoint, pushed so local and remote v1 align.
	baseCP := createCheckpointedCommit(t, env, "base work", "base.go", "package base", "base work")
	require.NotEmpty(t, baseCP)
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")
	require.True(t, env.CheckpointExistsOnRemote(bareDir, baseCP), "base checkpoint should be on remote")

	// Both sides add a checkpoint from the shared base -> diverged v1.
	_ = createCheckpointedCommit(t, env, "local diverge", "diverge.go", "package diverge", "local diverge")
	_ = advanceRemoteV1(t, env, bareDir, "remote diverge work")

	// A pushable feature commit so git actually invokes the hook.
	env.WriteFile("trigger.txt", "x")
	env.GitAdd("trigger.txt")
	env.GitCommit("push trigger")

	// Record the remote's feature-branch tip before the aborted push so we can
	// prove the push transferred nothing (the branch itself was seeded by
	// SetupBareRemote, so its mere presence is not evidence either way).
	tipBefore := remoteBranchTip(t, bareDir, "feature/test-branch")

	out, err := env.GitPushArgsWithHooks("origin", "HEAD")
	require.Error(t, err, "a diverged v1 under OPF must abort the user push")
	require.Contains(t, out, "diverged",
		"the push failure should surface the actionable divergence message:\n%s", out)

	require.Equal(t, tipBefore, remoteBranchTip(t, bareDir, "feature/test-branch"),
		"an aborted OPF push must not advance the user branch on the remote")
}

// TestOPFPrePush_BootstrapCapAbortsPush is E4: a first push to a remote with no
// v1 yet, whose local history exceeds ENTIRE_OPF_BOOTSTRAP_LIMIT, aborts with a
// typed BootstrapTooLargeError before OPF runs, and nothing is pushed. The cap
// is set to 1 while creating two checkpoints, so the second push's unpushed set
// exceeds it.
func TestOPFPrePush_BootstrapCapAbortsPush(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	marker := filepath.Join(t.TempDir(), "opf-invocations")
	enableOPF(t, env, writeFakeOPFScript(t, marker))
	// Cap the bootstrap at 1 commit; two checkpoints (2 v1 commits) exceed it.
	env.ExtraEnv = append(env.ExtraEnv, "ENTIRE_OPF_BOOTSTRAP_LIMIT=1")

	_ = createCheckpointedCommit(t, env, "first work", "one.go", "package one", "first work")
	_ = createCheckpointedCommit(t, env, "second work", "two.go", "package two", "second work")

	out, err := env.GitPushArgsWithHooks("origin", "HEAD")
	require.Error(t, err, "an over-cap OPF bootstrap must abort the push")
	require.Contains(t, out, "bootstrap",
		"the push failure should surface the bootstrap-cap remediation message:\n%s", out)

	require.False(t, env.CheckpointsPresentOnRemote(bareDir),
		"no checkpoints should reach the remote when the bootstrap cap aborts the push")

	// OPF itself is never invoked: the cap fires before the shell-out.
	_, statErr := os.Stat(marker)
	require.True(t, os.IsNotExist(statErr),
		"the bootstrap cap must abort before the opf binary is ever executed")
}

// TestOPFPrePush_NonTTY_CompletesWithoutHang is E5 (regression 626a0344e): a
// non-interactive push with OPF enabled must complete without any /dev/tty
// access and without hanging. GitPushArgsWithHooks runs git via execx.NonInteractive,
// which puts the child in a new session with no controlling terminal, so any
// stray /dev/tty read would fail rather than block. The push must finish well
// inside a generous wall-clock bound and land the trailer.
func TestOPFPrePush_NonTTY_CompletesWithoutHang(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	marker := filepath.Join(t.TempDir(), "opf-invocations")
	enableOPF(t, env, writeFakeOPFScript(t, marker))

	_ = createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")

	start := time.Now()
	out, err := env.GitPushArgsWithHooks("origin", "HEAD")
	elapsed := time.Since(start)

	require.NoError(t, err, "non-TTY OPF push should succeed without a controlling terminal:\n%s", out)
	require.Less(t, elapsed, 60*time.Second,
		"non-TTY OPF push must not hang on a /dev/tty read (took %s)", elapsed)
	require.Contains(t, remoteMetadataTipMessage(t, bareDir), "Entire-OPF-Applied: true",
		"the non-TTY OPF push should still rewrite and push the trailer")
}
