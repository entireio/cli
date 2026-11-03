package strategy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestCheckpointPushOutcomes(t *testing.T) {
	t.Parallel()
	const (
		pushedRef  = plumbing.ReferenceName("refs/entire/checkpoints/aa/aa0000000001")
		waitingRef = plumbing.ReferenceName("refs/entire/checkpoints/bb/bb0000000001")
		absentRef  = plumbing.ReferenceName("refs/entire/checkpoints/cc/cc0000000001")
	)
	tip := plumbing.NewHash("1111111111111111111111111111111111111111")
	drainErr := errors.New("drain push queue: disk full")
	waiting := map[plumbing.ReferenceName]struct{}{pushedRef: {}, waitingRef: {}}
	targets := []CheckpointPushRequest{{Ref: pushedRef}, {Ref: waitingRef}, {Ref: absentRef}}

	tests := []struct {
		name     string
		result   checkpointFlushResult
		flushErr error
		want     []CheckpointPushStatus
	}{
		{
			name:   "a waiting ref the flush did not report left the queue meanwhile",
			result: checkpointFlushResult{pushed: []checkpointRefPush{{name: pushedRef, hash: tip}}},
			want:   []CheckpointPushStatus{CheckpointPushUploaded, CheckpointPushNotWaiting, CheckpointPushNotWaiting},
		},
		{
			name:     "a flush-wide error fails every waiting ref it did not report",
			result:   checkpointFlushResult{pushed: []checkpointRefPush{{name: pushedRef, hash: tip}}},
			flushErr: drainErr,
			want:     []CheckpointPushStatus{CheckpointPushUploaded, CheckpointPushFailed, CheckpointPushNotWaiting},
		},
		{
			name:     "a flush that failed before reporting anything",
			flushErr: drainErr,
			want:     []CheckpointPushStatus{CheckpointPushFailed, CheckpointPushFailed, CheckpointPushNotWaiting},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outcomes := checkpointPushOutcomes(targets, waiting, nil, tt.result, tt.flushErr)
			got := make([]CheckpointPushStatus, 0, len(outcomes))
			for _, outcome := range outcomes {
				got = append(got, outcome.Status)
				if outcome.Status == CheckpointPushFailed {
					require.ErrorIs(t, outcome.Err, drainErr)
				}
			}
			require.Equal(t, tt.want, got)
		})
	}
}

// The OPF decision is made from the refs waiting when the command looked. A
// named ref queued after that must not be sent, or it would skip OPF.
func TestWaitingTargetSelection_LeavesOutRefsNotWaiting(t *testing.T) {
	t.Parallel()
	const (
		waitingRef = plumbing.ReferenceName("refs/entire/checkpoints/aa/aa0000000001")
		laterRef   = plumbing.ReferenceName("refs/entire/checkpoints/bb/bb0000000001")
	)
	pin := plumbing.NewHash("1111111111111111111111111111111111111111")
	waiting := map[plumbing.ReferenceName]struct{}{waitingRef: {}}

	selection, err := waitingTargetSelection([]CheckpointPushRequest{{Ref: waitingRef, Commit: pin}, {Ref: laterRef}}, waiting)
	require.NoError(t, err)

	got, ok := selection.pin(waitingRef)
	require.True(t, ok)
	require.Equal(t, pin, got)
	_, ok = selection.pin(laterRef)
	require.False(t, ok, "a ref that was not waiting is not sent")

	none, err := waitingTargetSelection([]CheckpointPushRequest{{Ref: laterRef}}, waiting)
	require.NoError(t, err)
	_, ok = none.pin(waitingRef)
	require.False(t, ok, "selecting nothing must not fall back to selecting every ref")
}

func TestWaitingTargetSelection_RefusesTwoCommitsForOneRef(t *testing.T) {
	t.Parallel()
	const ref = plumbing.ReferenceName("refs/entire/checkpoints/aa/aa0000000001")
	_, err := waitingTargetSelection([]CheckpointPushRequest{
		{Ref: ref, Commit: plumbing.NewHash("1111111111111111111111111111111111111111")},
		{Ref: ref, Commit: plumbing.NewHash("2222222222222222222222222222222222222222")},
	}, map[plumbing.ReferenceName]struct{}{ref: {}})
	require.Error(t, err)
}

// pushOfCommit is a pre-push line sending commit under a new branch name.
func pushOfCommit(commit string) []PrePushRef {
	return []PrePushRef{{LocalRef: commit, LocalSHA: commit, RemoteRef: "refs/heads/copy", RemoteSHA: plumbing.ZeroHash.String()}}
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", rev))
}

func enqueueInRepoAt(t *testing.T, dir string, refs ...plumbing.ReferenceName) {
	t.Helper()
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)
}

func TestCheckOuterPushOfHeldCommits_RefusesCurrentVersionOnly(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	enqueueInRepoAt(t, workDir, refs[0])
	older := revParse(t, workDir, refs[0].String())
	testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
	current := revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", refs[0].String(), current)

	err := checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(current), "test holds it")
	require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints, "the current version is held")

	require.NoError(t, checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(older), "test holds it"),
		"an older version is not matched")
}

func TestCheckOuterPushOfHeldCommits_PackedRefs(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	enqueueInRepoAt(t, workDir, refs...)
	testutil.RunGit(t, workDir, "pack-refs", "--all")

	err := checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(revParse(t, workDir, refs[1].String())), "test holds it")
	require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints)
}

// The queued name resolves only through a case-insensitive filesystem, so the
// push is refused exactly when that name reaches the ref.
func TestCheckOuterPushOfHeldCommits_CaseFoldedShard(t *testing.T) {
	workDir, _, _ := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "folded checkpoint")
	tip := revParse(t, workDir, "HEAD")
	stored := checkpoint.CheckpointRefPrefix + "AB/0000000000ab"
	testutil.RunGit(t, workDir, "update-ref", stored, tip)
	queued := mustRefName(t, id.MustCheckpointID("0000000000ab"))
	enqueueInRepoAt(t, workDir, queued)
	_, statErr := os.Stat(filepath.Join(workDir, ".git", filepath.FromSlash(queued.String())))

	err := checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(tip), "test holds it")

	if statErr == nil {
		require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints)
	} else {
		require.NoError(t, err)
	}
}

func TestCheckOuterPushOfHeldCommits_SHA256Repository(t *testing.T) {
	workDir := t.TempDir()
	testutil.RunGit(t, workDir, "init", "--object-format=sha256")
	testutil.RunGit(t, workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	tip := revParse(t, workDir, "HEAD")
	require.Len(t, tip, 64)
	ref := mustRefName(t, id.MustCheckpointID("000000000001"))
	testutil.RunGit(t, workDir, "update-ref", ref.String(), tip)
	t.Chdir(workDir)
	enqueueInRepoAt(t, workDir, ref)

	err := checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(tip), "test holds it")
	require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints)
}

func TestListOutgoingCheckpoints_ReportsEachTip(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(map[bool]string{false: "loose", true: "packed"}[packed], func(t *testing.T) {
			workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
			prepareGitRefsPrePush(t, workDir, bareDir)
			testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
			moved := revParse(t, workDir, "HEAD")
			testutil.RunGit(t, workDir, "update-ref", refs[1].String(), moved)
			stale := mustRefName(t, id.MustCheckpointID("ffffffffffff"))
			enqueueInRepoAt(t, workDir, refs[0], stale, refs[1])
			if packed {
				testutil.RunGit(t, workDir, "pack-refs", "--all")
			}

			out, err := ListOutgoingCheckpoints(t.Context())
			require.NoError(t, err)

			require.Equal(t, []OutgoingCheckpoint{
				{CheckpointID: id.MustCheckpointID("000000000000"), Ref: refs[0], Commit: plumbing.NewHash(revParse(t, workDir, "HEAD~1"))},
				{CheckpointID: id.MustCheckpointID("000000000001"), Ref: refs[1], Commit: plumbing.NewHash(moved)},
			}, out.Checkpoints)
			require.Equal(t, 1, out.StaleQueueEntries)
		})
	}
}

// Refs the listing omits keep today's tip lookup: a case-folded shard resolves
// on a case-insensitive filesystem, and an unparsable ref stays outgoing with
// no known tip.
func TestListOutgoingCheckpoints_UnlistedRefs(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	tip := revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", checkpoint.CheckpointRefPrefix+"AB/0000000000ab", tip)
	folded := mustRefName(t, id.MustCheckpointID("0000000000ab"))
	garbage := mustRefName(t, id.MustCheckpointID("eeeeeeeeeee2"))
	garbagePath := filepath.Join(workDir, ".git", filepath.FromSlash(garbage.String()))
	require.NoError(t, os.MkdirAll(filepath.Dir(garbagePath), 0o755))
	require.NoError(t, os.WriteFile(garbagePath, []byte("not a hash\n"), 0o600))
	enqueueInRepoAt(t, workDir, folded, garbage)
	_, statErr := os.Stat(filepath.Join(workDir, ".git", filepath.FromSlash(folded.String())))

	out, err := ListOutgoingCheckpoints(t.Context())
	require.NoError(t, err)

	want := []OutgoingCheckpoint{{CheckpointID: id.MustCheckpointID("eeeeeeeeeee2"), Ref: garbage}}
	if statErr == nil {
		want = append([]OutgoingCheckpoint{{CheckpointID: id.MustCheckpointID("0000000000ab"), Ref: folded, Commit: plumbing.NewHash(tip)}}, want...)
	}
	require.Equal(t, want, out.Checkpoints)
}
