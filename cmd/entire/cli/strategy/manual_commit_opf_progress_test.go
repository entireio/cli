package strategy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestPrePush_OPFRewritePhase_GitBranchBackend_ReportsProgress is a real
// reproduction of issue #1683 on the git-branch checkpoint backend, which is
// the exact scenario the issue's example transcript shows ("[entire] Pushing
// entire/checkpoints/v1 to origin..." followed only by dots).
//
// It builds two REAL v1 checkpoints through the real checkpoint store,
// enables OPF through a real .entire/settings.json, fakes only the external
// `opf` binary dependency via the repo's own documented test double
// (configureFakeOPF / fakeOPFForRewrite — see manual_commit_opf_rewrite_test.go),
// and invokes the REAL pre-push hook handler (ManualCommitStrategy.PrePush)
// against a real local bare "origin" remote (no mocked call). It then
// captures the real stderr the OPF phase writes.
//
// Before the fix: the only line ever written to opfPrePushProgressWriter is
// the existing non-TTY decision notice ("OpenAI Privacy Filter: scanning
// checkpoints before push"); the actual redaction work that follows — which
// is where the real wall-clock time goes — is completely silent, with no
// phase label and no commit count.
//
// After the fix: a second, distinct line reports the phase transition into
// the re-redaction work itself, naming how many checkpoint commits it
// covers, and is closed out with "done" once the (faked) OPF batch call
// returns.
func TestPrePush_OPFRewritePhase_GitBranchBackend_ReportsProgress(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, paths.EntireDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, paths.EntireDir, "settings.json"), []byte(`{
  "enabled": true,
  "redaction": {
    "openai_privacy_filter": {
      "enabled": true,
      "categories": {"private_person": true}
    }
  }
}`), 0o644))

	// Real local bare remote (no network) — same pattern used by the
	// existing TestPrePush_OPFProgressUsesConfiguredWriter.
	remoteDir := filepath.Join(t.TempDir(), "origin.git")
	_, err := git.PlainInit(remoteDir, true)
	require.NoError(t, err)
	testutil.AddRemote(t, tmpDir, "origin", remoteDir)
	t.Chdir(tmpDir)

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	// Two real checkpoints -> two real unpushed v1 commits that still need
	// OPF, so the reported count is exercising real multi-commit plumbing,
	// not a single-commit edge case.
	addV1Checkpoint(t, repo, "a1b2c3d4e5f6", "sess-1", "Hello, PERSONABC asked", "Look up PERSONABC")
	addV1Checkpoint(t, repo, "b2c3d4e5f6a1", "sess-2", "Hi, PERSONABC replied", "Find PERSONABC too")

	configureFakeOPF(t, &fakeOPFForRewrite{})

	var out bytes.Buffer
	withOPFPrePushProgressWriterForTest(t, &out)

	require.NoError(t, (&ManualCommitStrategy{}).PrePush(t.Context(), "origin"))

	captured := out.String()
	t.Logf("captured OPF progress stderr (git-branch backend):\n%s", captured)

	require.Contains(t, captured, "OpenAI Privacy Filter: scanning checkpoints before push",
		"the existing pre-run notice must still fire")
	// Two checkpoint writes against a fresh v1 store land as 3 real commits
	// (an initial root-metadata commit plus one per session write) — asserted
	// against the real observed count rather than the intuitive "2", so this
	// pins actual behavior instead of a guess.
	require.Contains(t, captured, "Re-redacting 3 checkpoint commit(s) via OpenAI Privacy Filter",
		"the actual re-redaction phase must be named and counted, not left silent")
}

// TestPrePushFromGitHook_OPFRewritePhase_GitRefsBackend_ReportsProgress is the
// git-refs backend's counterpart: same real hook handler contract
// (ManualCommitStrategy.PrePushFromGitHook), same real fake-OPF double, but
// exercising RewriteQueuedCheckpointRefsWithOPF (the git-refs sibling of the
// v1 rewrite) via setupGitRefsOPFRepo, the existing helper this package's own
// OPF-refs tests already use for a real git-refs-backend repo with real
// queued checkpoint refs.
func TestPrePushFromGitHook_OPFRewritePhase_GitRefsBackend_ReportsProgress(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	// Two real checkpoint refs, each carrying one unpushed, un-applied
	// commit that needs OPF.
	setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")

	var out bytes.Buffer
	withOPFPrePushProgressWriterForTest(t, &out)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	captured := out.String()
	t.Logf("captured OPF progress stderr (git-refs backend):\n%s", captured)

	require.Contains(t, captured, "Re-redacting 2 checkpoint commit(s) via OpenAI Privacy Filter",
		"the git-refs backend's re-redaction phase must be named and counted, not left silent")
}
