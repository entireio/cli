//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestPostCommitHook_EmptyTreeCommit_LeavesCheckpointsAlone covers the #2111
// aftermath through the real post-commit hook.
//
// A commit whose tree is empty while the files it removes are still on disk is
// what git writes when it reads `.git/index` as missing — ENOENT is silently
// treated as an empty index — and the user's recovery is `git reset --mixed
// HEAD~1`. Entire must not condense against such a commit: condensing deletes
// the shadow branch, marks the session condensed, and advances BaseCommit onto
// a commit that is about to stop existing, so the recovery leaves the session's
// pending checkpoint data consumed by a commit that no longer exists.
//
// Verified RED against the unguarded binary: `should_condense: true`, "session
// condensed", "shadow branch deleted".
func TestPostCommitHook_EmptyTreeCommit_LeavesCheckpointsAlone(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()

	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Create file A", sess.TranscriptPath))

	env.WriteFile("fileA.go", pkgFuncA)
	sess.CreateTranscript("Create file A", []FileChange{{Path: "fileA.go", Content: pkgFuncA}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))

	// A second turn start puts the session back into ACTIVE, the phase in which
	// post-commit condenses immediately — the state a mid-turn commit lands in,
	// and the one where an empty-tree commit costs the session its shadow
	// branch. Without the guard, this hook logs "session condensed" and "shadow
	// branch deleted" for the commit below.
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Now commit it", sess.TranscriptPath))

	shadowBranch := env.GetShadowBranchName()
	require.True(t, env.BranchExists(shadowBranch),
		"the session must have shadow-branch content for the hook to be able to lose")
	// The v1 branch already exists from session setup, so the signal that
	// condensation ran is its tip moving, not its existence.
	v1Before := branchHash(t, env, paths.MetadataBranchName)

	// The corrupt commit, with a checkpoint trailer so the unguarded hook would
	// have every reason to condense against it. The worktree is untouched —
	// files on disk plus no files in the commit is the whole signature.
	commitEmptyTreeWithTrailer(t, env, "a1b2c3d4e5f6")
	require.True(t, env.FileExists("fileA.go"), "the files must still be on disk")

	cmd := exec.CommandContext(t.Context(), getTestBinary(), "hooks", "git", "post-commit")
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the hook must not fail the user's commit:\n%s", out)
	t.Logf("post-commit hook output:\n%s", out)

	require.Contains(t, string(out), "records an EMPTY tree",
		"the hook must say what happened")
	require.Contains(t, string(out), "git reset --mixed HEAD~1",
		"the hook must name the recovery")

	require.True(t, env.BranchExists(shadowBranch),
		"shadow branch must survive: the user is about to reset this commit away")
	require.Equal(t, v1Before, branchHash(t, env, paths.MetadataBranchName),
		"nothing may be condensed onto entire/checkpoints/v1 for an empty-tree commit")
}

// branchHash returns the branch tip, or "" when the branch does not exist.
func branchHash(t *testing.T, env *TestEnv, branch string) string {
	t.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	defer repo.Close()

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

// commitEmptyTreeWithTrailer moves HEAD to a new commit whose tree is the empty
// tree, the shape git writes when it reads the index as empty. Built through
// plumbing because no porcelain path produces it without also deleting the
// files from the worktree, and their presence on disk is half the signature.
func commitEmptyTreeWithTrailer(t *testing.T, env *TestEnv, checkpointIDStr string) {
	t.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	defer repo.Close()

	head, err := repo.Head()
	require.NoError(t, err)

	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, (&object.Tree{}).Encode(treeObj))
	emptyTree, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	sig := object.Signature{Name: "Test User", Email: "test@example.com", When: time.Now()}
	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      "record the user's work\n\n" + trailers.CheckpointTrailerKey + ": " + id.MustCheckpointID(checkpointIDStr).String() + "\n",
		TreeHash:     emptyTree,
		ParentHashes: []plumbing.Hash{head.Hash()},
	}
	commitObj := repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(commitObj))
	hash, err := repo.Storer.SetEncodedObject(commitObj)
	require.NoError(t, err)

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(head.Name(), hash)))
}

// TestPrepareCommitMsgHook_EmptyIndex_WarnsBeforeTheCommit covers the #2111
// guard on the near side of the commit, through the real prepare-commit-msg
// hook of the real binary.
//
// It exists because the wiring is the part that can silently disappear: a
// mutation deleting the guard's call from the hook's RunE left the unit tests,
// the cli package tests, and the whole integration suite green. Testing the
// detector is not testing the hook.
//
// The index here records zero entries while every tracked file is still on
// disk — the state git leaves behind when it reads `.git/index` as missing —
// produced with real git so the bytes are the ones git writes.
func TestPrepareCommitMsgHook_EmptyIndex_WarnsBeforeTheCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	testutil.RunGit(t, env.RepoDir, "rm", "-r", "--cached", "-q", ".")
	require.True(t, env.FileExists("README.md"), "the tracked files must still be on disk")

	out := runPrepareCommitMsgHook(t, env)
	require.Contains(t, out, "is about to record an EMPTY tree",
		"the hook must warn before the commit object exists")
	require.Contains(t, out, "git reset --mixed HEAD~1",
		"the hook must name the recovery")
}

// TestPrepareCommitMsgHook_PopulatedIndex_IsSilent is the other half: the guard
// must not announce itself on an ordinary commit.
func TestPrepareCommitMsgHook_PopulatedIndex_IsSilent(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.WriteFile("fileA.go", pkgFuncA)
	env.GitAdd("fileA.go")

	require.NotContains(t, runPrepareCommitMsgHook(t, env), "EMPTY tree",
		"an ordinary staged commit must produce no warning")
}

// runPrepareCommitMsgHook invokes the real binary as git would, including the
// GIT_INDEX_FILE git exports to its commit hooks — the relative `.git/index`
// measured from real commits — and fails the test if the hook errors, since a
// git hook of Entire's may never fail a user's commit.
func runPrepareCommitMsgHook(t *testing.T, env *TestEnv) string {
	t.Helper()

	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte("record the user's work\n"), 0o644))

	cmd := exec.CommandContext(t.Context(), getTestBinary(),
		"hooks", "git", "prepare-commit-msg", msgFile, "message")
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"GIT_INDEX_FILE=.git/index")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the hook must not fail the user's commit:\n%s", out)
	t.Logf("prepare-commit-msg hook output:\n%s", out)
	return string(out)
}
