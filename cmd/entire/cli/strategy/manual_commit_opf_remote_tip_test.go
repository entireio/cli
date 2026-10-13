package strategy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// remoteHistoryContains reports whether any commit reachable from ref on the
// bare remote carries needle in its content.
func remoteHistoryContains(t *testing.T, bareDir string, ref plumbing.ReferenceName, needle string) bool {
	t.Helper()
	return strings.Contains(testutil.RunGit(t, bareDir, "log", "-p", "--text", ref.String()), needle)
}

// An untrailered commit another clone published (OPF off) must not reach a
// second remote unscanned. Recovery replays this clone's trailered commit on
// top of it, so the tip carries the trailer while the history below it does
// not; a later push of the same ref to another remote must still scan that
// history, because that remote has never received it.
func TestPrePushCheckpointRefs_UnscannedHistoryDoesNotReachASecondRemote(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	originDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	ref := refs[0]
	wt, err := repo.Worktree()
	require.NoError(t, err)
	workDir := wt.Filesystem().Root()
	runGit := func(args ...string) string { return strings.TrimSpace(testutil.RunGit(t, workDir, args...)) }

	// Another clone, OPF off, published the sentinel-bearing commit to origin.
	foreign := refHashes(t, repo, refs)[0]
	runGit("push", "-q", originDir, foreign.String()+":"+ref.String())
	require.True(t, remoteHistoryContains(t, originDir, ref, "PERSONABC"))

	// This clone holds its own, already-scanned generation of the same ref.
	testutil.WriteFile(t, workDir, "safe.txt", "nothing personal")
	testutil.GitAdd(t, workDir, "safe.txt")
	tree := runGit("write-tree")
	local := plumbing.NewHash(runGit("commit-tree", tree, "-m", trailers.AppendOPFAppliedTrailer("local generation")))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, local)))
	enqueueRefs(t, repo, refs)

	captureStderrWriter(t)
	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()

	// A later write to the same checkpoint, then the queue delivered to a second
	// destination (a checkpoint_remote configured after the first push): the
	// same rewrite and flush the pre-push hook runs.
	addGitRefsSessionWithTranscript(t, repo, "a1b2c3d4e5f6", "sess-later", "a later turn")
	forkDir := filepath.Join(t.TempDir(), "fork.git")
	_, err = git.PlainInit(forkDir, true)
	require.NoError(t, err)
	restore = captureStderr(t)
	fork := pushSettings{remote: forkDir}
	delivery := newOPFDelivery(t.Context(), repo, fork, OPFRun)
	require.NoError(t, rewriteCheckpointRefsForPush(t.Context(), repo, delivery))
	_, _, flushErr := flushCheckpointRefsQueue(t.Context(), repo, fork, delivery)
	restore()
	require.NoError(t, flushErr)

	require.NotEmpty(t, remoteRefHash(t, forkDir, ref), "the ref must reach the second remote")
	require.False(t, remoteHistoryContains(t, forkDir, ref, "PERSONABC"),
		"unscanned history published only to origin must not reach fork")
}

// History the destination already holds is not this push's to rewrite. When
// the local ref sits on another clone's untrailered commit that the remote
// already has, a later write must ship as one commit on top of it: rewriting
// the published commit too would make the push non-fast-forward, and recovery
// would then replay the rewritten copy onto the remote, landing it twice.
func TestPrePushCheckpointRefs_PublishedHistoryIsNotRewrittenAgain(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	originDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	ref := refs[0]
	wt, err := repo.Worktree()
	require.NoError(t, err)
	workDir := wt.Filesystem().Root()

	// Another clone, OPF off, published this checkpoint; this clone holds the
	// same commit (fetched, or moved onto it by recovery).
	published := refHashes(t, repo, refs)[0]
	testutil.RunGit(t, workDir, "push", "-q", originDir, published.String()+":"+ref.String())
	queue := enqueueRefs(t, repo, nil)
	require.NoError(t, queue.RemoveEntries(mustPeekEntries(t, queue)))

	addGitRefsSessionWithTranscript(t, repo, "a1b2c3d4e5f6", "sess-later", "a later turn")
	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()

	tip := remoteRefHash(t, originDir, ref)
	require.Equal(t, "1", strings.TrimSpace(testutil.RunGit(t, originDir, "rev-list", "--count", published.String()+".."+tip)),
		"the later write must land as exactly one commit on top of the published one")
	tipCommit, err := repo.CommitObject(plumbing.NewHash(tip))
	require.NoError(t, err)
	require.True(t, trailers.HasOPFApplied(tipCommit.Message), "the new commit is scanned")
}

func mustPeekEntries(t *testing.T, q interface {
	PeekEntries() ([]checkpoint.PushQueueEntry, error)
}) []checkpoint.PushQueueEntry {
	t.Helper()
	entries, err := q.PeekEntries()
	require.NoError(t, err)
	return entries
}

// Delivery judges every commit a push would send, not the tip alone: a
// trailered tip on an unscanned commit the destination lacks is held back, and
// ships once the destination is known to hold that commit.
func TestPartitionCheckpointRefPushes_ChecksEveryUnsentCommit(t *testing.T) {
	_, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	ref := refs[0]
	unscanned := refHashes(t, repo, refs)[0]
	wt, err := repo.Worktree()
	require.NoError(t, err)
	workDir := wt.Filesystem().Root()
	tree := strings.TrimSpace(testutil.RunGit(t, workDir, "rev-parse", unscanned.String()+"^{tree}"))
	tip := plumbing.NewHash(strings.TrimSpace(testutil.RunGit(t, workDir, "commit-tree", tree, "-p", unscanned.String(),
		"-m", trailers.AppendOPFAppliedTrailer("replayed on top"))))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, tip)))
	entries := []checkpoint.PushQueueEntry{{Ref: ref, Hash: tip}}

	ready, awaiting, _, _ := partitionCheckpointRefPushes(repo, entries, &opfDelivery{})
	require.Empty(t, ready, "the unscanned commit below the trailered tip would be sent")
	require.Len(t, awaiting, 1)

	held := &opfDelivery{remoteTips: map[plumbing.ReferenceName]plumbing.Hash{ref: unscanned}}
	ready, awaiting, _, _ = partitionCheckpointRefPushes(repo, entries, held)
	require.Len(t, ready, 1, "the destination already holds the unscanned commit")
	require.Empty(t, awaiting)
}

// triangularRemote adds a remote whose fetch URL and push URL name different
// bare repositories, the pushurl topology, and returns both.
func triangularRemote(t *testing.T, workDir, name string) (fetchDir, pushDir string) {
	t.Helper()
	fetchDir = filepath.Join(t.TempDir(), "fetch.git")
	pushDir = filepath.Join(t.TempDir(), "push.git")
	for _, dir := range []string{fetchDir, pushDir} {
		_, err := git.PlainInit(dir, true)
		require.NoError(t, err)
	}
	testutil.RunGit(t, workDir, "remote", "add", name, fetchDir)
	testutil.RunGit(t, workDir, "remote", "set-url", "--push", name, pushDir)
	return fetchDir, pushDir
}

// The destination's tips come from the repository the push writes to, not the
// one the remote's name reads from: judging delivery by the fetch repository
// would treat history as held that the push repository never received.
func TestOPFDestinationTips_ReadsTheRepositoryThePushWritesTo(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	fetchDir, pushDir := triangularRemote(t, workDir, "dest")
	runGit := func(args ...string) string { return strings.TrimSpace(testutil.RunGit(t, workDir, args...)) }

	tree := runGit("rev-parse", "HEAD^{tree}")
	fetchTip := runGit("commit-tree", tree, "-m", "only in the fetch repository")
	pushTip := runGit("commit-tree", tree, "-m", "only in the push repository")
	runGit("push", "-q", fetchDir, fetchTip+":"+refs[0].String())
	runGit("push", "-q", pushDir, pushTip+":"+refs[0].String())

	tips := opfDestinationTips(ctx, pushSettings{remote: "dest"})
	require.Equal(t, pushTip, tips[refs[0]].String())
}

// A destination that cannot be read yields no tips, so every ref's whole
// history counts as unsent.
func TestOPFDestinationTips_UnreadableDestinationIsUnknown(t *testing.T) {
	workDir, _, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)

	tips := opfDestinationTips(context.Background(), pushSettings{remote: filepath.Join(t.TempDir(), "missing.git")})
	require.Nil(t, tips)
}

// Recovery replays onto what the push repository holds. Fetching the remote's
// fetch repository instead finds a different history (here none), so the
// replay cannot land and the ref never ships.
func TestPushCheckpointRefWithRecovery_ReplaysOntoThePushRepository(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	ref := refs[0]
	_, pushDir := triangularRemote(t, workDir, "dest")
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	runGit := func(args ...string) string { return strings.TrimSpace(testutil.RunGit(t, workDir, args...)) }

	tree := runGit("rev-parse", "HEAD^{tree}")
	theirs := runGit("commit-tree", tree, "-p", "HEAD", "-m", trailers.AppendOPFAppliedTrailer("another clone's write"))
	runGit("push", "-q", pushDir, theirs+":"+ref.String())
	runGit("update-ref", "-d", ref.String())
	testutil.WriteFile(t, workDir, "ours.txt", "this clone's write")
	testutil.GitAdd(t, workDir, "ours.txt")
	ours := plumbing.NewHash(runGit("commit-tree", runGit("write-tree"), "-p", "HEAD", "-m", trailers.AppendOPFAppliedTrailer("this clone's write")))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, ours)))

	restore := captureStderr(t)
	delivered, err := pushCheckpointRefWithRecovery(ctx, repo, "dest", checkpointRefPush{name: ref, hash: ours}, &opfDelivery{})
	restore()
	require.NoError(t, err)
	require.Equal(t, delivered.hash.String(), remoteRefHash(t, pushDir, ref))
	require.Equal(t, theirs, runGit("rev-parse", delivered.hash.String()+"^"), "the replay sits on the push repository's tip")
}

// A replay whose new commits lack the trailer is not sent: recovery copies this
// clone's commits onto the remote, and the copies carry their content unscanned.
func TestPushCheckpointRefWithRecovery_RefusesAnUnscannedReplay(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	ref := refs[0]
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	runGit := func(args ...string) string { return strings.TrimSpace(testutil.RunGit(t, workDir, args...)) }

	tree := runGit("rev-parse", "HEAD^{tree}")
	theirs := runGit("commit-tree", tree, "-p", "HEAD", "-m", trailers.AppendOPFAppliedTrailer("another clone's write"))
	runGit("push", "-q", bareDir, theirs+":"+ref.String())
	testutil.WriteFile(t, workDir, "ours.txt", "PERSONABC")
	testutil.GitAdd(t, workDir, "ours.txt")
	ours := plumbing.NewHash(runGit("commit-tree", runGit("write-tree"), "-p", "HEAD", "-m", "written with OPF off"))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, ours)))

	restore := captureStderr(t)
	_, err = pushCheckpointRefWithRecovery(ctx, repo, bareDir, checkpointRefPush{name: ref, hash: ours}, &opfDelivery{})
	restore()
	require.ErrorContains(t, err, "without the OPF trailer")
	require.Equal(t, theirs, remoteRefHash(t, bareDir, ref), "the remote must not move")
}
