//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// D2 / D3 -- git-refs per-checkpoint ref divergence & recovery (integration).
//
// The unit-level recovery is covered in strategy/refs_push_test.go
// (TestBatchPushRefs_RejectsNonFastForward, TestPushCheckpointRefWithRecovery_
// MergesDivergedRef). These drive the same behaviour end-to-end through the real
// pre-push path (drain the push queue -> batch push -> per-ref fetch+replay
// recovery), where the checkpoint ref really lives on a bare remote.
// =============================================================================

// checkpointIdentityEnv is a git env with a committer/author identity, needed by
// commit-tree in a bare remote (which has no repo-local user config).
func checkpointIdentityEnv() []string {
	return append(testutil.GitIsolatedEnv(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
}

// amendCheckpointRef advances the checkpoint ref in the repo at dir to a new
// child commit of base that adds fileName. It builds the commit with plumbing +
// a temporary index so no working tree is required (works in a bare remote too),
// then updates the ref. Returns the new commit hash. This models another clone
// (or the bare's own state) advancing a per-checkpoint ref.
func amendCheckpointRef(t *testing.T, dir, ref, base, fileName, content string) string {
	t.Helper()

	idxPath := filepath.Join(t.TempDir(), "amend-index")
	run := func(stdin string, args ...string) string {
		c := exec.CommandContext(t.Context(), "git", args...)
		c.Dir = dir
		c.Env = append(checkpointIdentityEnv(), "GIT_INDEX_FILE="+idxPath)
		if stdin != "" {
			c.Stdin = strings.NewReader(stdin)
		}
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v in %s failed: %s", args, dir, out)
		return strings.TrimSpace(string(out))
	}

	run("", "read-tree", base)
	blobSHA := run(content, "hash-object", "-w", "--stdin")
	run("", "update-index", "--add", "--cacheinfo", "100644,"+blobSHA+","+fileName)
	treeSHA := run("", "write-tree")
	commitSHA := run("", "commit-tree", treeSHA, "-p", base, "-m", "amend "+fileName)
	run("", "update-ref", ref, commitSHA)
	return commitSHA
}

// enqueueCheckpointRef appends ref to the git-refs push queue in the repo's git
// common dir, so the next pre-push drains and pushes it. Mirrors what a checkpoint
// write does internally.
func enqueueCheckpointRef(t *testing.T, env *TestEnv, ref string) {
	t.Helper()

	commonDir := gitCommonDir(t, env.RepoDir)
	queuePath := filepath.Join(commonDir, "entire-checkpoint-push-queue.jsonl")
	f, err := os.OpenFile(queuePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(`{"ref":"` + ref + `"}` + "\n")
	require.NoError(t, err)
}

// gitCommonDir resolves the git common dir for a repo/worktree dir.
func gitCommonDir(t *testing.T, dir string) string {
	t.Helper()
	c := exec.CommandContext(t.Context(), "git", "rev-parse", "--git-common-dir")
	c.Dir = dir
	c.Env = testutil.GitIsolatedEnv()
	out, err := c.Output()
	require.NoError(t, err, "resolve git common dir")
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return common
}

// refHashIn returns the commit hash a ref points at in the repo at dir.
func refHashIn(t *testing.T, dir, ref string) string {
	t.Helper()
	c := exec.CommandContext(t.Context(), "git", "rev-parse", ref)
	c.Dir = dir
	c.Env = testutil.GitIsolatedEnv()
	out, err := c.Output()
	require.NoError(t, err, "rev-parse %s in %s", ref, dir)
	return strings.TrimSpace(string(out))
}

// refTreeFilesIn lists the files in the tree a ref points at in the repo at dir.
func refTreeFilesIn(t *testing.T, dir, ref string) string {
	t.Helper()
	c := exec.CommandContext(t.Context(), "git", "ls-tree", "-r", "--name-only", ref)
	c.Dir = dir
	c.Env = testutil.GitIsolatedEnv()
	out, err := c.CombinedOutput()
	require.NoError(t, err, "ls-tree %s in %s: %s", ref, dir, out)
	return string(out)
}

// isAncestorIn reports whether ancestor is an ancestor of descendant in dir.
func isAncestorIn(t *testing.T, dir, ancestor, descendant string) bool {
	t.Helper()
	c := exec.CommandContext(t.Context(), "git", "merge-base", "--is-ancestor", ancestor, descendant)
	c.Dir = dir
	c.Env = testutil.GitIsolatedEnv()
	return c.Run() == nil
}

// fetchCheckpointRefLocally fetches a single checkpoint ref from origin into the
// same ref name locally (so a clone can then amend and diverge it).
func fetchCheckpointRefLocally(t *testing.T, env *TestEnv, ref string) {
	t.Helper()
	c := exec.CommandContext(t.Context(), "git", "fetch", "--no-tags", "origin", ref+":"+ref)
	c.Dir = env.RepoDir
	c.Env = testutil.GitIsolatedEnv()
	out, err := c.CombinedOutput()
	require.NoError(t, err, "fetch checkpoint ref %s: %s", ref, out)
}

// TestGitRefsDivergence_RemoteAheadRefFetchReplaysNonForce is D2's primary case:
// the remote checkpoint ref was amended by another clone (a valid child commit).
// A local divergent write of the SAME ref then pushes through the real pre-push
// path: the fast-forward-only batch push is rejected, recovery fetches the remote
// tip and replays the local-only change on top (non-force), so BOTH sides' files
// survive and the remote's commit is preserved as an ancestor.
func TestGitRefsDivergence_RemoteAheadRefFetchReplaysNonForce(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()

	cp := createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
	require.NotEmpty(t, cp)
	ref := checkpointRefName(cp)

	// Push the checkpoint ref to the remote (remote ref = C0).
	env.RunPrePush("origin")
	require.True(t, env.CheckpointExistsOnRemote(bareDir, cp))
	c0 := refHashIn(t, bareDir, ref)

	// Another clone amends the same checkpoint on the remote: remote ref -> C_remote
	// (a child of C0 that adds remote_side.txt).
	cRemote := amendCheckpointRef(t, bareDir, ref, c0, "remote_side.txt", "remote change")

	// Locally, diverge the same ref to a sibling child of C0 (adds local_side.txt)
	// and enqueue it for the next push.
	amendCheckpointRef(t, env.RepoDir, ref, c0, "local_side.txt", "local change")
	enqueueCheckpointRef(t, env, ref)

	// Real pre-push path: batch push is non-fast-forward -> recovery fetch+replay.
	env.RunPrePush("origin")

	// Both sides' files survive on the remote ref.
	files := refTreeFilesIn(t, bareDir, ref)
	require.Contains(t, files, "remote_side.txt", "remote-only change must be preserved (non-force)")
	require.Contains(t, files, "local_side.txt", "local-only change must be replayed on top")

	// Non-force: the remote's amended commit remains an ancestor of the new tip.
	newRemote := refHashIn(t, bareDir, ref)
	require.True(t, isAncestorIn(t, bareDir, cRemote, newRemote),
		"the remote's amended commit must be preserved as an ancestor, never overwritten")

	// The queue was drained by the successful recovery push.
	require.Empty(t, env.PushQueueRefs(), "push queue should be drained after the recovery push landed")
}

// TestGitRefsDivergence_RejectedRefStaysQueuedRemoteUntouched is D2's queue-safety
// case: when the remote refuses the checkpoint ref update, the ref is left queued
// and the remote ref is untouched (never force-overwritten); a later push, once
// the remote accepts writes again, lands it.
//
// A pure content-level cherry-pick conflict is not reproducible at this layer
// (the tree-delta apply is last-writer, not a 3-way merge), so a server-side
// rejection (a bare pre-receive hook that declines the checkpoint namespace)
// stands in for "the ref could not land" — exercising the same "stays queued,
// remote untouched, retry next time" property.
func TestGitRefsDivergence_RejectedRefStaysQueuedRemoteUntouched(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()

	cp := createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
	require.NotEmpty(t, cp)
	ref := checkpointRefName(cp)

	env.RunPrePush("origin")
	require.True(t, env.CheckpointExistsOnRemote(bareDir, cp))
	remoteBefore := refHashIn(t, bareDir, ref)

	// Reject any update to the checkpoint ref namespace on the remote.
	installRejectCheckpointRefsHook(t, bareDir)

	// Advance the local ref (a fast-forward child) and enqueue it.
	amendCheckpointRef(t, env.RepoDir, ref, remoteBefore, "local_side.txt", "local change")
	enqueueCheckpointRef(t, env, ref)

	// Pre-push: the push is rejected, recovery cannot make it land -> the ref is
	// left queued and the remote ref is unchanged (never force-overwritten).
	env.RunPrePush("origin")
	require.Contains(t, env.PushQueueRefs(), ref, "a rejected ref must stay queued for a later push")
	require.Equal(t, remoteBefore, refHashIn(t, bareDir, ref),
		"a rejected push must leave the remote ref untouched (no force overwrite)")

	// Once the remote accepts writes again, the next pre-push retries and lands it.
	removeRejectCheckpointRefsHook(t, bareDir)
	env.RunPrePush("origin")
	require.Contains(t, refTreeFilesIn(t, bareDir, ref), "local_side.txt",
		"the queued ref must land on the next push once the remote accepts it")
	require.Empty(t, env.PushQueueRefs(), "queue should drain after the retry succeeds")
}

// installRejectCheckpointRefsHook writes a pre-receive hook on the bare remote
// that declines any update to refs/entire/checkpoints/*.
func installRejectCheckpointRefsHook(t *testing.T, bareDir string) {
	t.Helper()
	hooksDir := filepath.Join(bareDir, "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	script := "#!/bin/sh\n" +
		"while read _old _new ref; do\n" +
		"  case \"$ref\" in\n" +
		"    refs/entire/checkpoints/*) echo 'rejected: checkpoint refs are protected' >&2; exit 1;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "pre-receive"), []byte(script), 0o755)) //nolint:gosec // hook must be executable
}

func removeRejectCheckpointRefsHook(t *testing.T, bareDir string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(bareDir, "hooks", "pre-receive")))
}

// TestGitRefsConcurrentPush_SecondPusherReplaysAndRetries is D3: two clones race
// on the SAME per-checkpoint ref (each amends it from a shared base). The first
// push fast-forwards; the second is non-fast-forward and must fetch+replay+retry
// (non-force), so both clones' changes survive. This is the git-refs analogue of
// TestConcurrentPush_SecondPusherRebasesAndRetries (v1-only).
func TestGitRefsConcurrentPush_SecondPusherReplaysAndRetries(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()

	cp := createCheckpointedCommit(t, env, "Add shared module", "shared.go", "package shared", "Add shared module")
	require.NotEmpty(t, cp)
	ref := checkpointRefName(cp)
	env.RunPrePush("origin")
	c0 := refHashIn(t, bareDir, ref)

	// Two clones both fetch the shared checkpoint ref at C0 (before either pushes).
	cloneA := env.CloneFrom(bareDir)
	cloneA.CheckpointStore = StoreGitRefs
	fetchCheckpointRefLocally(t, cloneA, ref)

	cloneB := env.CloneFrom(bareDir)
	cloneB.CheckpointStore = StoreGitRefs
	fetchCheckpointRefLocally(t, cloneB, ref)

	// Each clone amends the same ref differently and enqueues it.
	amendCheckpointRef(t, cloneA.RepoDir, ref, c0, "a_side.txt", "A change")
	enqueueCheckpointRef(t, cloneA, ref)
	amendCheckpointRef(t, cloneB.RepoDir, ref, c0, "b_side.txt", "B change")
	enqueueCheckpointRef(t, cloneB, ref)

	// A pushes first (fast-forward over C0).
	cloneA.RunPrePush("origin")
	require.Contains(t, refTreeFilesIn(t, bareDir, ref), "a_side.txt")

	// B pushes second: non-fast-forward -> fetch+replay+retry, preserving both.
	cloneB.RunPrePush("origin")
	files := refTreeFilesIn(t, bareDir, ref)
	require.Contains(t, files, "a_side.txt", "clone A's change must be preserved (not overwritten)")
	require.Contains(t, files, "b_side.txt", "clone B's change must be replayed on top")
	require.Empty(t, cloneB.PushQueueRefs(), "clone B's queue should drain after the recovery push")
}
