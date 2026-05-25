package strategy

// These tests focus on the FetchMetadataBranch behavior when the local
// entire/checkpoints/v1 ref diverges from (or has no shared history with) the
// remote tip. They are paired with the existing fast-forward / no-rewind
// contract tests in checkpoint_remote_test.go:
//
//   TestFetchMetadataBranch_FetchesAndCreatesLocalBranch  -- missing local
//   TestFetchMetadataBranch_UpdatesExistingLocalBranch    -- local strictly behind
//   TestFetchMetadataBranch_DoesNotRewindLocalAhead       -- local strictly ahead
//
// The scenarios here cover the cases above and beyond fast-forward:
//   - local diverged from remote (sibling commits sharing a base)
//   - local disconnected from remote (no common ancestor)
//   - multi-commit local chains
//   - empty-tree orphan-root pollution
//   - shallow-clone boundaries that hide ancestry
//
// Several of these tests document behavior introduced by the in-flight
// "replay local checkpoints when fetch finds a diverged remote" work and will
// fail against the current main contract (which preserves but does not replay
// diverged local refs). That is intentional -- they serve as an executable
// specification of the target behavior.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- shared scaffolding ----------------------------------------------------

// checkpointBranchEnv holds the two repos a fetch test needs: a "remote"
// where the entire/checkpoints/v1 orphan branch is built, and a "local" that
// will run FetchMetadataBranch against it.
type checkpointBranchEnv struct {
	t             *testing.T
	ctx           context.Context
	remoteDir     string
	localDir      string
	defaultBranch string
}

// newCheckpointBranchEnv initializes both repos with a trivial commit on the
// default branch so they have an established working-branch HEAD that fetches
// won't disturb.
func newCheckpointBranchEnv(t *testing.T) *checkpointBranchEnv {
	t.Helper()
	ctx := context.Background()

	remoteDir := t.TempDir()
	testutil.InitRepo(t, remoteDir)
	testutil.WriteFile(t, remoteDir, "f.txt", "init")
	testutil.GitAdd(t, remoteDir, "f.txt")
	testutil.GitCommit(t, remoteDir, "init")

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	defaultBranch := repoCurrentBranch(ctx, t, remoteDir)
	require.NotEmpty(t, defaultBranch, "default branch should be set on fresh repo")
	require.Equal(t, defaultBranch, repoCurrentBranch(ctx, t, localDir),
		"remote and local should share the same default branch name for predictable test setup")

	return &checkpointBranchEnv{
		t:             t,
		ctx:           ctx,
		remoteDir:     remoteDir,
		localDir:      localDir,
		defaultBranch: defaultBranch,
	}
}

func repoCurrentBranch(ctx context.Context, t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// createCheckpointOrphanRoot creates a fresh entire/checkpoints/v1 orphan
// branch in repoDir with a single root commit containing a metadata.json file.
// Returns the commit hash. Leaves HEAD on the default branch.
func createCheckpointOrphanRoot(ctx context.Context, t *testing.T, repoDir, defaultBranch, label string) string {
	t.Helper()

	runGit(ctx, t, repoDir, "checkout", "--orphan", paths.MetadataBranchName)
	runGit(ctx, t, repoDir, "rm", "-rf", ".")

	contents := fmt.Sprintf(`{"checkpoint": %q}`, label)
	testutil.WriteFile(t, repoDir, "metadata.json", contents)
	testutil.GitAdd(t, repoDir, "metadata.json")
	runGit(ctx, t, repoDir, "-c", "commit.gpgsign=false", "commit", "-m", "checkpoint "+label)

	hash := getMetadataBranchHash(ctx, t, repoDir)
	runGit(ctx, t, repoDir, "checkout", defaultBranch)
	return hash
}

// appendCheckpointCommit adds a checkpoint commit on top of the current
// metadata-branch tip in repoDir. fileName/contents distinguish commits in
// assertions. Returns the new commit hash. HEAD is restored to defaultBranch.
func appendCheckpointCommit(ctx context.Context, t *testing.T, repoDir, defaultBranch, fileName, contents string) string {
	t.Helper()

	runGit(ctx, t, repoDir, "checkout", paths.MetadataBranchName)
	testutil.WriteFile(t, repoDir, fileName, contents)
	testutil.GitAdd(t, repoDir, fileName)
	runGit(ctx, t, repoDir, "-c", "commit.gpgsign=false", "commit", "-m", "checkpoint "+fileName)

	hash := getMetadataBranchHash(ctx, t, repoDir)
	runGit(ctx, t, repoDir, "checkout", defaultBranch)
	return hash
}

// createOrphanRootWithEmptyTree creates an empty-tree root commit on the
// metadata branch in repoDir -- reproduces the historical "orphan-bug" commit
// shape that filtering logic must skip during replay. Returns the commit
// hash. Leaves HEAD on defaultBranch.
func createOrphanRootWithEmptyTree(ctx context.Context, t *testing.T, repoDir, defaultBranch string) string {
	t.Helper()

	runGit(ctx, t, repoDir, "checkout", "--orphan", paths.MetadataBranchName)
	runGit(ctx, t, repoDir, "rm", "-rf", ".")
	runGit(ctx, t, repoDir, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "empty orphan root")

	hash := getMetadataBranchHash(ctx, t, repoDir)
	runGit(ctx, t, repoDir, "checkout", defaultBranch)
	return hash
}

func runGit(ctx context.Context, t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s failed: %s", strings.Join(args, " "), out)
}

func getMetadataBranchHash(ctx context.Context, t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", paths.MetadataBranchName)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoErrorf(t, err, "git rev-parse %s failed in %s", paths.MetadataBranchName, repoDir)
	return strings.TrimSpace(string(out))
}

// metadataBranchReachable returns every commit reachable from the metadata
// branch tip in repoDir. Order is newest-first, matching git rev-list default.
func metadataBranchReachable(ctx context.Context, t *testing.T, repoDir string) []string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "rev-list", paths.MetadataBranchName)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoErrorf(t, err, "git rev-list %s failed in %s", paths.MetadataBranchName, repoDir)
	hashes := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(hashes) == 1 && hashes[0] == "" {
		return nil
	}
	return hashes
}

// readBlobOnMetadataBranch returns the contents of `path` on the metadata
// branch tip in repoDir, or "" if absent.
func readBlobOnMetadataBranch(ctx context.Context, t *testing.T, repoDir, path string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "show", paths.MetadataBranchName+":"+path)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// ---- Group A1 -------------------------------------------------------------

// TestFetchMetadataBranch_DivergedLocal_ReplaysOntoRemote verifies that when
// the local entire/checkpoints/v1 ref has diverged from the remote tip (the
// two share a common ancestor but neither is an ancestor of the other),
// FetchMetadataBranch replays the local-only commits onto the fetched remote
// tip, so a single ref includes both histories.
//
// On the current main contract (PR #1252), the fetch is a no-op for diverged
// refs and local stays at its sibling commit -- this test will fail there.
// It locks the target behavior introduced by the in-flight replay work.
//
// Not parallel: uses t.Chdir().
func TestFetchMetadataBranch_DivergedLocal_ReplaysOntoRemote(t *testing.T) {
	env := newCheckpointBranchEnv(t)
	ctx := env.ctx

	// Build a shared base commit A on the remote's metadata branch.
	aHash := createCheckpointOrphanRoot(ctx, t, env.remoteDir, env.defaultBranch, "A")

	// Local fetches once so it has A.
	t.Chdir(env.localDir)
	require.NoError(t, FetchMetadataBranch(ctx, env.remoteDir))
	require.Equal(t, aHash, getMetadataBranchHash(ctx, t, env.localDir),
		"setup: local must start at the same commit as remote")

	// Local advances to B (a sibling commit, no push).
	bHash := appendCheckpointCommit(ctx, t, env.localDir, env.defaultBranch, "local-b.json", `{"side":"local"}`)
	require.NotEqual(t, aHash, bHash)

	// Remote advances to C (a different sibling on top of A) -- as if another
	// machine pushed in between.
	cHash := appendCheckpointCommit(ctx, t, env.remoteDir, env.defaultBranch, "remote-c.json", `{"side":"remote"}`)
	require.NotEqual(t, aHash, cHash)
	require.NotEqual(t, bHash, cHash)

	// Fetch again. The local ref must now expose both B's and C's data.
	require.NoError(t, FetchMetadataBranch(ctx, env.remoteDir))

	tipHash := getMetadataBranchHash(ctx, t, env.localDir)

	// C must be reachable from the new tip (remote work preserved).
	reachable := metadataBranchReachable(ctx, t, env.localDir)
	assert.Contains(t, reachable, cHash,
		"after diverged-fetch replay, remote tip C should be reachable from local; tip=%s", tipHash)

	// B's tree contents must be present in the tip's tree (local work preserved).
	localBlob := readBlobOnMetadataBranch(ctx, t, env.localDir, "local-b.json")
	assert.Contains(t, localBlob, "local",
		"after diverged-fetch replay, local-b.json must appear in the merged tip tree")

	// C's tree contents must also be present.
	remoteBlob := readBlobOnMetadataBranch(ctx, t, env.localDir, "remote-c.json")
	assert.Contains(t, remoteBlob, "remote",
		"after diverged-fetch replay, remote-c.json must appear in the merged tip tree")
}

// ---- Group A2 -------------------------------------------------------------

// TestFetchMetadataBranch_DisconnectedEmptyRoot_FiltersOrphan reproduces the
// review's critical finding C1: when the local metadata branch starts with an
// empty-tree root (the historical "orphan-bug" commit shape) and continues
// with a real-content commit, replay-onto-target must skip the empty root.
//
// Without filtering, the empty root's tree could collide with real entries on
// the fetched tip; with filtering, the empty root is dropped and only the
// real commit is replayed.
//
// Will fail on main (no replay happens) and on a config-perm-shape patch that
// forgets to apply the filter (the empty root cannot be detected after the
// fact). The right shape filters at the cherry-pick boundary -- see
// ReconcileDisconnectedMetadataBranch for the reference filter.
//
// Not parallel: uses t.Chdir().
func TestFetchMetadataBranch_DisconnectedEmptyRoot_FiltersOrphan(t *testing.T) {
	env := newCheckpointBranchEnv(t)
	ctx := env.ctx

	// Remote: completely independent metadata branch (R).
	rHash := createCheckpointOrphanRoot(ctx, t, env.remoteDir, env.defaultBranch, "R")

	// Local: empty-tree orphan root, then a real commit on top.
	rootHash := createOrphanRootWithEmptyTree(ctx, t, env.localDir, env.defaultBranch)
	realHash := appendCheckpointCommit(ctx, t, env.localDir, env.defaultBranch,
		"local-real.json", `{"side":"local","keep":true}`)
	require.NotEqual(t, rootHash, realHash)

	// Sanity: local and remote share no commits.
	require.NotEqual(t, rHash, rootHash)
	require.NotEqual(t, rHash, realHash)

	t.Chdir(env.localDir)
	require.NoError(t, FetchMetadataBranch(ctx, env.remoteDir))

	tipHash := getMetadataBranchHash(ctx, t, env.localDir)
	reachable := metadataBranchReachable(ctx, t, env.localDir)
	assert.Contains(t, reachable, rHash,
		"remote tip R should be reachable after replay onto disconnected target; tip=%s", tipHash)

	// The real local file must be present.
	realBlob := readBlobOnMetadataBranch(ctx, t, env.localDir, "local-real.json")
	assert.Contains(t, realBlob, "keep",
		"replay must carry the real local commit forward (orphan-empty-root must not block it)")

	// Sanity: the new tip's reachable set must not include the empty-tree root.
	// The orphan root is detached and serves no purpose after replay.
	assert.NotContains(t, reachable, rootHash,
		"empty-tree orphan root should not appear in the merged history; it must be filtered before replay")
}

// ---- Group A3 -------------------------------------------------------------

// TestFetchMetadataBranch_DisconnectedMultiCommit_PreservesAllLocalData
// verifies that a multi-commit local chain (3 commits, each adding a distinct
// file) is fully replayed onto the disconnected remote tip with no data loss
// at any intermediate commit.
//
// Today's diverged/disconnected tests only ever exercise a single local-only
// commit; this guards against an off-by-one in chain traversal or a
// "skip empty deltas" branch (cherryPickOnto's `len(changes) == 0` continue)
// that could drop a commit whose net tree change happens to be a no-op.
//
// Not parallel: uses t.Chdir().
func TestFetchMetadataBranch_DisconnectedMultiCommit_PreservesAllLocalData(t *testing.T) {
	env := newCheckpointBranchEnv(t)
	ctx := env.ctx

	// Remote: single root R, disconnected from local. Use a distinct filename
	// ("remote-only.json") so we can verify remote data is reachable from the
	// merged tip -- the preserve-on-divergence path leaves the test passing
	// vacuously on local data alone, so we anchor the assertion on remote.
	rHash := createCheckpointOrphanRoot(ctx, t, env.remoteDir, env.defaultBranch, "R")
	appendCheckpointCommit(ctx, t, env.remoteDir, env.defaultBranch,
		"remote-only.json", `{"side":"remote"}`)

	// Local: three-commit chain L1 -> L2 -> L3, each adding a distinct file.
	createCheckpointOrphanRoot(ctx, t, env.localDir, env.defaultBranch, "L1")
	appendCheckpointCommit(ctx, t, env.localDir, env.defaultBranch, "l2.json", `{"step":2}`)
	appendCheckpointCommit(ctx, t, env.localDir, env.defaultBranch, "l3.json", `{"step":3}`)

	t.Chdir(env.localDir)
	require.NoError(t, FetchMetadataBranch(ctx, env.remoteDir))

	tipHash := getMetadataBranchHash(ctx, t, env.localDir)
	reachable := metadataBranchReachable(ctx, t, env.localDir)
	assert.Contains(t, reachable, rHash,
		"remote root R should be reachable from the merged tip after multi-commit replay; tip=%s", tipHash)

	// All three local files must be readable from the merged tip's tree.
	root := readBlobOnMetadataBranch(ctx, t, env.localDir, "metadata.json")
	assert.Contains(t, root, "L1", "L1's metadata.json must survive replay")

	step2 := readBlobOnMetadataBranch(ctx, t, env.localDir, "l2.json")
	assert.Contains(t, step2, "2", "L2's file must survive replay")

	step3 := readBlobOnMetadataBranch(ctx, t, env.localDir, "l3.json")
	assert.Contains(t, step3, "3", "L3's file must survive replay")

	// And the remote-only file must also be reachable.
	remote := readBlobOnMetadataBranch(ctx, t, env.localDir, "remote-only.json")
	assert.Contains(t, remote, "remote", "remote-only.json must be reachable after multi-commit replay")
}

// ---- Group A4 -------------------------------------------------------------

// TestFetchMetadataBranch_ShallowClone_RefusesReplayAcrossBoundary verifies
// that when the local repo's history is shallow, FetchMetadataBranch refuses
// to replay across the shallow boundary. The algorithm cannot prove the two
// histories are disconnected (the shallow cut may hide a real merge base), so
// silently replaying would risk merging unrelated histories.
//
// Setup: build a remote with a metadata branch chain R1 -> R2 -> R3, then
// shallow-clone it (depth=1) so the local repo sees only R3 with no parents
// and no shallow-aware ancestry info reaching back. Build a local-only
// commit L on top of R3, then advance the remote to a separate sibling R3'
// rooted differently so that local and remote diverge in a shape where the
// merge base is hidden behind the shallow boundary.
//
// Expectation: the fetch returns an error referencing shallow history, OR it
// leaves the local ref unchanged (no silent merge). It must NOT layer local's
// tree onto remote's tip without ancestry proof.
//
// Not parallel: uses t.Chdir().
func TestFetchMetadataBranch_ShallowClone_RefusesReplayAcrossBoundary(t *testing.T) {
	env := newCheckpointBranchEnv(t)
	ctx := env.ctx

	// Remote: orphan checkpoint chain R1 -> R2 -> R3 on the metadata branch.
	createCheckpointOrphanRoot(ctx, t, env.remoteDir, env.defaultBranch, "R1")
	appendCheckpointCommit(ctx, t, env.remoteDir, env.defaultBranch, "r2.json", `{"step":2}`)
	r3Hash := appendCheckpointCommit(ctx, t, env.remoteDir, env.defaultBranch, "r3.json", `{"step":3}`)

	// Local repo: do a shallow fetch of just the metadata branch tip.
	// This gives local the R3 commit but cuts ancestry at the shallow boundary.
	shallowFetchArgs := []string{
		"fetch", "--no-tags", "--depth=1",
		env.remoteDir,
		"+refs/heads/" + paths.MetadataBranchName + ":refs/heads/" + paths.MetadataBranchName,
	}
	runGit(ctx, t, env.localDir, shallowFetchArgs...)
	require.Equal(t, r3Hash, getMetadataBranchHash(ctx, t, env.localDir))

	// Local: append a commit on top of R3. This is the unpushed local work.
	lHash := appendCheckpointCommit(ctx, t, env.localDir, env.defaultBranch, "l.json", `{"side":"local"}`)
	require.NotEqual(t, r3Hash, lHash)

	// Remote: advance its own metadata branch with a separate orphan-style
	// rewrite so the new remote tip is unreachable from L through R3. (Force
	// the remote to a brand-new orphan chain.)
	runGit(ctx, t, env.remoteDir, "update-ref", "-d", "refs/heads/"+paths.MetadataBranchName)
	createCheckpointOrphanRoot(ctx, t, env.remoteDir, env.defaultBranch, "R-prime")

	t.Chdir(env.localDir)
	err := FetchMetadataBranch(ctx, env.remoteDir)

	if err == nil {
		// Acceptable alternative: the fetch refused to replay and left local
		// pointing at its own L commit (the PR #1252 preserve-on-divergence
		// behavior, which is also safe).
		afterHash := getMetadataBranchHash(ctx, t, env.localDir)
		assert.Equal(t, lHash, afterHash,
			"shallow-bounded divergence must not silently merge; either return an error or leave local untouched (got tip %s, expected L=%s)",
			afterHash, lHash)
		return
	}

	// If an error came back, it must mention shallow / history / unshallow so
	// the user can act on it. The exact phrasing can evolve, but it must not
	// be a raw internal sentinel.
	msg := err.Error()
	assert.Truef(t,
		strings.Contains(msg, "shallow") ||
			strings.Contains(msg, "unshallow") ||
			strings.Contains(msg, "history"),
		"shallow-replay error should mention shallow/history/unshallow, got: %s", msg)
}

// ---- Group E1 -------------------------------------------------------------

// TestFetchMetadataBranch_ShallowBoundary_ErrorIsActionable verifies that
// when FetchMetadataBranch refuses to act because shallow history hides the
// merge base, any error returned points the user at a remediation step
// (running `entire doctor` or `git fetch --unshallow`). Without this, users
// hit the wall with a phrase like "reachable shallow history prevents proving
// refs are disconnected" and have no idea what to do (review M3).
//
// This test deliberately permits the alternate safe behavior (no error,
// local untouched) -- it only asserts that *if* an error surfaces, it
// includes guidance.
//
// Not parallel: uses t.Chdir().
func TestFetchMetadataBranch_ShallowBoundary_ErrorIsActionable(t *testing.T) {
	env := newCheckpointBranchEnv(t)
	ctx := env.ctx

	createCheckpointOrphanRoot(ctx, t, env.remoteDir, env.defaultBranch, "R1")
	r2Hash := appendCheckpointCommit(ctx, t, env.remoteDir, env.defaultBranch, "r2.json", `{"step":2}`)

	shallowFetchArgs := []string{
		"fetch", "--no-tags", "--depth=1",
		env.remoteDir,
		"+refs/heads/" + paths.MetadataBranchName + ":refs/heads/" + paths.MetadataBranchName,
	}
	runGit(ctx, t, env.localDir, shallowFetchArgs...)
	require.Equal(t, r2Hash, getMetadataBranchHash(ctx, t, env.localDir))

	appendCheckpointCommit(ctx, t, env.localDir, env.defaultBranch, "l.json", `{"side":"local"}`)

	runGit(ctx, t, env.remoteDir, "update-ref", "-d", "refs/heads/"+paths.MetadataBranchName)
	createCheckpointOrphanRoot(ctx, t, env.remoteDir, env.defaultBranch, "R-prime")

	t.Chdir(env.localDir)
	err := FetchMetadataBranch(ctx, env.remoteDir)

	if err == nil {
		t.Skip("fetch returned no error; no message to validate (the alternate safe path)")
	}

	msg := err.Error()
	hasActionable := strings.Contains(msg, "doctor") ||
		strings.Contains(msg, "--unshallow") ||
		strings.Contains(msg, "unshallow ") ||
		strings.Contains(msg, "fetch --depth")
	assert.Truef(t, hasActionable,
		"shallow-boundary fetch error must include a user-actionable hint (entire doctor / git fetch --unshallow); got: %s", msg)
}
