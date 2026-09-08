package strategy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestOPFV1RewriteDoesNotLoseConcurrentCheckpointUpdate(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	repoRoot, repo, oldTip := setupV1RepoInDir(t)
	tree := emptyTreeHash(t, repo)
	nativeTip := makeOrphanCommit(t, repo, tree, []plumbing.Hash{oldTip}, "native checkpoint write")
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)

	commitNative := prepareNativeRefUpdate(t, repoRoot, refName, oldTip, nativeTip)
	_, opfErr := RewriteUnpushedV1WithOPF(t.Context(), repo, "origin")
	commitNative()

	finalRef, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.Equal(t, nativeTip, finalRef.Hash())
	var movedErr *V1RefMovedError
	require.NotErrorAs(t, opfErr, &movedErr,
		"a held lock does not prove that the ref moved")
	require.ErrorIs(t, opfErr, gitrepo.ErrRefLocked,
		"the OPF rewrite must report ref contention while a native update owns the ref lock")
}

func TestAtomicSetV1Ref_ReportsActualCASConflictAsMoved(t *testing.T) {
	t.Parallel()
	_, repo, oldTip := setupV1RepoInDir(t)
	tree := emptyTreeHash(t, repo)
	newTip := makeOrphanCommit(t, repo, tree, []plumbing.Hash{oldTip}, "OPF rewrite")
	externalTip := makeOrphanCommit(t, repo, tree, []plumbing.Hash{oldTip}, "concurrent checkpoint write")
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, externalTip)))

	err := atomicSetV1Ref(t.Context(), repo, oldTip, newTip)

	var movedErr *V1RefMovedError
	require.ErrorAs(t, err, &movedErr)
	require.Equal(t, &V1RefMovedError{Expected: oldTip, Actual: externalTip}, movedErr)
	require.NotErrorIs(t, err, gitrepo.ErrRefLocked)
}

func TestAtomicSetV1Ref_DoesNotReportUnverifiedConflictAsMoved(t *testing.T) {
	t.Parallel()
	repoRoot, repo, oldTip := setupV1RepoInDir(t)
	tree := emptyTreeHash(t, repo)
	newTip := makeOrphanCommit(t, repo, tree, []plumbing.Hash{oldTip}, "OPF rewrite")
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	require.NoError(t, repo.Storer.RemoveReference(refName))
	require.NoError(t, os.MkdirAll(
		filepath.Join(repoRoot, ".git", filepath.FromSlash(refName.String())),
		0o755,
	))

	err := atomicSetV1Ref(t.Context(), repo, oldTip, newTip)

	var movedErr *V1RefMovedError
	require.NotErrorAs(t, err, &movedErr,
		"an unreadable current value does not prove that the ref moved")
	require.Error(t, err)
	require.NotErrorIs(t, err, gitrepo.ErrRefCASConflict)
	require.Contains(t, err.Error(), "unable to resolve reference")
}

func TestOPFCheckpointRefRewriteDoesNotLoseConcurrentCheckpointUpdate(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	repoRoot := worktree.Filesystem().Root()
	refName := refs[0]
	oldRef, err := repo.Reference(refName, true)
	require.NoError(t, err)
	oldCommit, err := repo.CommitObject(oldRef.Hash())
	require.NoError(t, err)
	nativeTip := makeOrphanCommit(t, repo, oldCommit.TreeHash, []plumbing.Hash{oldRef.Hash()}, "native checkpoint write")

	commitNative := prepareNativeRefUpdate(t, repoRoot, refName, oldRef.Hash(), nativeTip)
	opfErr := RewriteQueuedCheckpointRefsWithOPF(t.Context(), repo)
	commitNative()

	finalRef, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.Equal(t, nativeTip, finalRef.Hash())
	require.ErrorIs(t, opfErr, gitrepo.ErrRefLocked,
		"the OPF rewrite must report ref contention while a native update owns the ref lock")
}

func prepareNativeRefUpdate(
	t *testing.T,
	repoRoot string,
	refName plumbing.ReferenceName,
	oldTip plumbing.Hash,
	newTip plumbing.Hash,
) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	cmd := exec.CommandContext(ctx, "git", "update-ref", "--stdin")
	cmd.Dir = repoRoot
	cmd.Env = testutil.GitIsolatedEnv()
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		if cmd.ProcessState == nil {
			if err := cmd.Wait(); err != nil {
				t.Logf("stopped unfinished git update-ref transaction: %v", err)
			}
		}
	})

	scanner := bufio.NewScanner(stdout)
	writeAndExpectGitUpdateRefResponse(t, stdin, scanner, "start", "start: ok")
	writeAndExpectGitUpdateRefResponse(t, stdin, scanner,
		fmt.Sprintf("update %s %s %s", refName, newTip, oldTip), "")
	writeAndExpectGitUpdateRefResponse(t, stdin, scanner, "prepare", "prepare: ok")

	return func() {
		writeAndExpectGitUpdateRefResponse(t, stdin, scanner, "commit", "commit: ok")
		require.NoError(t, stdin.Close())
		waitErr := cmd.Wait()
		require.NoError(t, waitErr, stderr.String())
	}
}

func writeAndExpectGitUpdateRefResponse(
	t *testing.T,
	stdin io.Writer,
	scanner *bufio.Scanner,
	command string,
	want string,
) {
	t.Helper()
	_, err := fmt.Fprintln(stdin, command)
	require.NoError(t, err)
	if want == "" {
		return
	}
	require.True(t, scanner.Scan(), "git update-ref ended before %q", want)
	require.Equal(t, want, scanner.Text())
}
