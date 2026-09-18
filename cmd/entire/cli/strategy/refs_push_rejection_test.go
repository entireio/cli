package strategy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const checkpointRejectReason = "push declined due to repository rule violations"
const checkpointUnblockURL = "https://github.com/example/checkpoints/security/secret-scanning/unblock-secret/2mQ8vR5xL9nT3bW7kP4sH6jY0cF1dZ"

// Like remote.TestPushWithOptions_ErrorCarriesRemoteRejectionReason, use a real
// bare remote's pre-receive hook, not a fake git executable.
func installCheckpointRejectHook(t *testing.T, bareDir string, longOutput bool) {
	t.Helper()
	message := "GITHUB PUSH PROTECTION: Amazon AWS Access Key ID; path: 0/full.jsonl:85; " + checkpointUnblockURL + "\n"
	if longOutput {
		message += strings.Repeat("additional repository policy detail\n", 200)
	}
	message += checkpointRejectReason
	hook := "#!/bin/sh\ncat >&2 <<'REASON'\n" + message + "\nREASON\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
}

func TestPushCheckpointRefWithRecovery_PreservesRejection(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir) // CWD-based push/recovery; cannot run in parallel.
	installCheckpointRejectHook(t, bareDir, false)

	err := pushCheckpointRefWithRecovery(t.Context(), bareDir, refs[0])
	require.ErrorContains(t, err, checkpointRejectReason)
	require.ErrorContains(t, err, checkpointUnblockURL)
	assert.NotContains(t, err.Error(), "sync diverged")
	assert.NotContains(t, err.Error(), "couldn't find remote ref", "confirmed rejection must skip recovery")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the original push error remains wrapped")
	assertRefsAbsentFromRemote(t, bareDir, refs, "blocked refs must not land")
}

func TestPushCheckpointRefWithRecovery_PreservesUnknownFailure(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)

	// Unknown failures still attempt recovery and must preserve both causes.
	err := pushCheckpointRefWithRecovery(t.Context(), filepath.Join(t.TempDir(), "missing.git"), refs[0])
	var recoveryErr *checkpointRefRecoveryError
	require.ErrorAs(t, err, &recoveryErr)
	require.ErrorIs(t, err, recoveryErr.pushErr)
	require.ErrorIs(t, err, recoveryErr.recoveryErr)
	require.ErrorContains(t, recoveryErr.pushErr, "git push")
	require.ErrorContains(t, recoveryErr.recoveryErr, "fetch failed")
	assert.NotContains(t, err.Error(), "sync diverged")
}

func TestPrePushCheckpointRefs_RejectionDoesNotRewriteExistingRef(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	testutil.RunGit(t, workDir, "remote", "add", "origin", bareDir)
	require.NoError(t, batchPushRefs(t.Context(), bareDir, refs[:1]))
	remoteTip := remoteRefHash(t, bareDir, refs[0])

	// Backfill a ref that already exists remotely. Pin the timestamp in the
	// past so replay's time.Now() deterministically changes the commit hash.
	t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00Z")
	testutil.WriteFile(t, workDir, "backfill.txt", "updated checkpoint content")
	testutil.GitAdd(t, workDir, "backfill.txt")
	tree := strings.TrimSpace(testutil.RunGit(t, workDir, "write-tree"))
	localTip := strings.TrimSpace(testutil.RunGit(t, workDir, "commit-tree", tree, "-p", "HEAD", "-m", "checkpoint backfill"))
	testutil.RunGit(t, workDir, "update-ref", refs[0].String(), localTip)
	installCheckpointRejectHook(t, bareDir, false)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs[:1])

	for range 2 {
		restore := captureStderr(t)
		err = NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin")
		output := restore()
		require.NoError(t, err, "checkpoint rejection must not block the user's push")
		assert.Contains(t, output, checkpointRejectReason)
		localRef, refErr := repo.Reference(refs[0], true)
		require.NoError(t, refErr)
		assert.Equal(t, localTip, localRef.Hash().String(), "a remote policy rejection must not replay local commits")
		assert.Equal(t, remoteTip, remoteRefHash(t, bareDir, refs[0]))
		remaining, drainErr := queue.Drain()
		require.NoError(t, drainErr)
		assert.Equal(t, refs[:1], remaining, "blocked update must remain queued")
	}
}

func TestPrePushCheckpointRefs_RejectionIsFailSoftAndVisibleOnce(t *testing.T) {
	for _, longOutput := range []bool{false, true} {
		name := "short"
		if longOutput {
			name = "elided"
		}
		t.Run(name, func(t *testing.T) {
			workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
			t.Chdir(workDir) // Also captures process-global stderr.
			paths.ClearWorktreeRootCache()
			t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
			testutil.RunGit(t, workDir, "remote", "add", "origin", bareDir)
			installCheckpointRejectHook(t, bareDir, longOutput)
			repo, err := gitrepo.OpenPath(workDir)
			require.NoError(t, err)
			defer repo.Close()
			queue := enqueueRefs(t, repo, refs)

			restore := captureStderr(t)
			err = NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin")
			output := restore()
			require.NoError(t, err, "checkpoint rejection must never block git push")
			assert.Equal(t, 1, strings.Count(output, checkpointRejectReason), output)
			assert.Contains(t, output, refs[0].String())
			assert.Contains(t, output, "Amazon AWS Access Key ID")
			assert.Contains(t, output, "0/full.jsonl:85")
			assert.Contains(t, output, checkpointUnblockURL)
			assert.Contains(t, output, "(showing one rejection):\nremote:", "label the single example separately from Git's output")
			assert.Contains(t, output, "\nremote:", "preserve the remote's line breaks")
			assert.NotContains(t, output, "git push: exit status", "terminal output should not include log error wrappers")
			assert.NotContains(t, output, "sync diverged")
			assert.NotContains(t, output, "couldn't find remote ref")
			assert.Less(t, len([]rune(output)), 3000, "reuse the remote layer's output cap")
			if longOutput {
				assert.Contains(t, output, "[…]")
			}
			remaining, err := queue.Drain()
			require.NoError(t, err)
			assert.ElementsMatch(t, refs, remaining)
			assertRefsAbsentFromRemote(t, bareDir, refs, "blocked refs must stay local")
		})
	}
}

func TestCheckpointRefRejectionReason_PreservesRemoteDetail(t *testing.T) {
	t.Parallel()
	// Synthetic token: even credential-shaped remote output must be preserved,
	// just as it would be displayed by a manual git push.
	token := "ghp_" + strings.Repeat("Ab12Cd34", 5)
	detail := "[remote rejected] hook declined; token=" + token
	assert.Equal(t, detail, checkpointRefRejectionReason(errors.New(detail)))
	assert.Equal(t, detail, checkpointRefRejectionReason(&checkpointRefRecoveryError{
		pushErr: errors.New(detail), recoveryErr: errors.New("fetch failed"),
	}))
}

func TestCheckpointRefRejectionReason_QuietWithoutRemoteRejection(t *testing.T) {
	t.Parallel()
	for _, detail := range []string{
		"! [rejected] refs/entire/checkpoints/AA/X (non-fast-forward)",
		"! [rejected] refs/entire/checkpoints/AA/X (fetch first)",
		"fatal: unable to access remote: Could not resolve host",
	} {
		t.Run(detail, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, checkpointRefRejectionReason(errors.New(detail)))
			assert.Empty(t, checkpointRefRejectionReason(&checkpointRefRecoveryError{
				pushErr: errors.New(detail), recoveryErr: errors.New("fetch failed"),
			}))
		})
	}
}
