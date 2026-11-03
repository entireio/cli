package strategy

import (
	"errors"
	"fmt"
	"os/exec"
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
	folds := refPathFoldsCase(t, workDir, queued)

	err := checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(tip), "test holds it")

	if folds {
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
	for _, tc := range []struct {
		name   string
		packed bool
	}{
		{"loose", false},
		{"packed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
			prepareGitRefsPrePush(t, workDir, bareDir)
			testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
			moved := revParse(t, workDir, "HEAD")
			testutil.RunGit(t, workDir, "update-ref", refs[1].String(), moved)
			stale := mustRefName(t, id.MustCheckpointID("ffffffffffff"))
			enqueueInRepoAt(t, workDir, refs[0], stale, refs[1])
			if tc.packed {
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

// createCheckpointRefs points n new checkpoint refs at target in one git
// update-ref transaction. Their IDs start past the refs
// setupRepoWithCheckpointRefs creates.
func createCheckpointRefs(t *testing.T, workDir string, n int, target string) []plumbing.ReferenceName {
	t.Helper()
	refs := make([]plumbing.ReferenceName, 0, n)
	var stdin strings.Builder
	for i := range n {
		ref := mustRefName(t, id.MustCheckpointID(fmt.Sprintf("%012x", 0x1000+i)))
		fmt.Fprintf(&stdin, "create %s %s\n", ref, target)
		refs = append(refs, ref)
	}
	cmd := exec.CommandContext(t.Context(), "git", "update-ref", "--stdin")
	cmd.Dir = workDir
	cmd.Env = testutil.GitIsolatedEnv()
	cmd.Stdin = strings.NewReader(stdin.String())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "update-ref --stdin: %s", out)
	return refs
}

// listedQueueShapes are the ref storage shapes a listed queue is checked on.
// The moved ref is the one whose newer version is written after the pack.
var listedQueueShapes = []struct { //nolint:gochecknoglobals // shared test table
	name                  string
	packBefore, packAfter bool
}{
	{"loose", false, false},
	{"loose over packed", true, false},
	{"packed", true, true},
}

// setupListedQueue queues enough refs to list the namespace. The last queued
// ref moves from older to current; every other ref stays at the root commit.
func setupListedQueue(t *testing.T, workDir string, packBefore, packAfter bool) (queued []plumbing.ReferenceName, older, current string) {
	t.Helper()
	root := revParse(t, workDir, "HEAD")
	queued = createCheckpointRefs(t, workDir, checkpointRefListingMinQueue, root)
	moved := queued[len(queued)-1]
	testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "older checkpoint version")
	older = revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", moved.String(), older)
	if packBefore {
		testutil.RunGit(t, workDir, "pack-refs", "--all")
	}
	testutil.RunGit(t, workDir, "commit", "--allow-empty", "-m", "newer checkpoint version")
	current = revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", moved.String(), current)
	if packAfter {
		testutil.RunGit(t, workDir, "pack-refs", "--all")
	}
	enqueueInRepoAt(t, workDir, queued...)
	return queued, older, current
}

func TestCheckOuterPushOfHeldCommits_ListedQueue(t *testing.T) {
	for _, shape := range listedQueueShapes {
		t.Run(shape.name, func(t *testing.T) {
			workDir, _, _ := setupRepoWithCheckpointRefs(t)
			t.Chdir(workDir)
			_, older, current := setupListedQueue(t, workDir, shape.packBefore, shape.packAfter)

			err := checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(current), "test holds it")
			require.ErrorIs(t, err, ErrOuterPushCarriesHeldCheckpoints, "the current version is held")

			require.NoError(t, checkOuterPushOfHeldCommits(t.Context(), pushOfCommit(older), "test holds it"),
				"an older version is not matched")
		})
	}
}

func TestListOutgoingCheckpoints_ListedQueue(t *testing.T) {
	for _, shape := range listedQueueShapes {
		t.Run(shape.name, func(t *testing.T) {
			workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
			prepareGitRefsPrePush(t, workDir, bareDir)
			queued, _, _ := setupListedQueue(t, workDir, shape.packBefore, shape.packAfter)
			stale := mustRefName(t, id.MustCheckpointID("ffffffffffff"))
			enqueueInRepoAt(t, workDir, stale)

			out, err := ListOutgoingCheckpoints(t.Context())
			require.NoError(t, err)

			want := make([]OutgoingCheckpoint, 0, len(queued))
			for _, ref := range queued {
				cid, ok := checkpoint.ParseRef(ref)
				require.True(t, ok)
				want = append(want, OutgoingCheckpoint{CheckpointID: cid, Ref: ref, Commit: plumbing.NewHash(revParse(t, workDir, ref.String()))})
			}
			require.Equal(t, want, out.Checkpoints)
			require.Equal(t, 1, out.StaleQueueEntries)
		})
	}
}

// Refs no listing names still get a tip lookup: a case-folded shard resolves
// on a case-insensitive filesystem, and an unparsable ref stays outgoing with
// no known tip.
func TestListOutgoingCheckpoints_UnlistedRefs(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithCheckpointRefs(t)
	prepareGitRefsPrePush(t, workDir, bareDir)
	tip := revParse(t, workDir, "HEAD")
	testutil.RunGit(t, workDir, "update-ref", checkpoint.CheckpointRefPrefix+"AB/0000000000ab", tip)
	folded := mustRefName(t, id.MustCheckpointID("0000000000ab"))
	garbage := mustRefName(t, id.MustCheckpointID("eeeeeeeeeee2"))
	writeRefFile(t, workDir, garbage, "not a hash\n")
	enqueueInRepoAt(t, workDir, folded, garbage)
	folds := refPathFoldsCase(t, workDir, folded)

	out, err := ListOutgoingCheckpoints(t.Context())
	require.NoError(t, err)

	want := []OutgoingCheckpoint{{CheckpointID: id.MustCheckpointID("eeeeeeeeeee2"), Ref: garbage}}
	if folds {
		want = append([]OutgoingCheckpoint{{CheckpointID: id.MustCheckpointID("0000000000ab"), Ref: folded, Commit: plumbing.NewHash(tip)}}, want...)
	}
	require.Equal(t, want, out.Checkpoints)
}
