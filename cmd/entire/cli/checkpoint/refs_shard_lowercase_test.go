package checkpoint

import (
	"context"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A legacy ID that shares foldableULID's shard bucket ("6b" / "6B").
const sharingLegacy = id.CheckpointID("a1b2c3d4e56b")

func TestLocalRefName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cid  id.CheckpointID
		want plumbing.ReferenceName
	}{
		{"ulid shard is lowercased", foldableULID, "refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B"},
		{"ulid shard outside hex is lowercased too", "01KVBJCWYA4YW6J5M9GP655HZN", "refs/entire/checkpoints/zn/01KVBJCWYA4YW6J5M9GP655HZN"},
		{"legacy shard is already lowercase", foldableLegacy, "refs/entire/checkpoints/f6/a1b2c3d4e5f6"},
		{"all-digit shard is unchanged", unfoldableLegacy, "refs/entire/checkpoints/12/a1b2c3d4e512"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := LocalRefName(tt.cid)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			parsed, ok := ParseRef(got)
			require.True(t, ok, "the local name must parse back to its checkpoint")
			assert.Equal(t, tt.cid, parsed)
		})
	}

	_, err := LocalRefName("not-an-id")
	assert.Error(t, err)
}

// refsStoreWithIgnoreCase is newRefsStore with core.ignorecase set, reopened
// the way the next CLI invocation would see the repository.
func refsStoreWithIgnoreCase(t *testing.T, ignoreCase string) *gitRefsStore {
	t.Helper()
	dir := repoDirOf(t, newRefsStore(t))
	testutil.RunGit(t, dir, "config", "core.ignorecase", ignoreCase)
	return reopenRefsStore(t, dir)
}

func reopenRefsStore(t *testing.T, dir string) *gitRefsStore {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return newGitRefsStore(repo)
}

func TestGitRefsStore_CaseFoldingRepoStoresNewCheckpointUnderLowercaseShard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := refsStoreWithIgnoreCase(t, "true")

	refsWrite(t, store, foldableULID, "sess-1", "transcript")

	assert.Equal(t, []string{"refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B"}, checkpointRefNames(t, store))
	testutil.RunGit(t, repoDirOf(t, store), "pack-refs", "--all")
	packed := reopenRefsStore(t, repoDirOf(t, store))
	summary, err := packed.Read(ctx, foldableULID)
	require.NoError(t, err)
	require.NotNil(t, summary, "the checkpoint must read under its lowercase name once packed")
}

func TestGitRefsStore_CaseSensitiveRepoKeepsTheCanonicalShard(t *testing.T) {
	t.Parallel()
	store := refsStoreWithIgnoreCase(t, "false")

	refsWrite(t, store, foldableULID, "sess-1", "transcript")

	assert.Equal(t, []string{mustRefName(t, foldableULID).String()}, checkpointRefNames(t, store),
		"where the filesystem cannot fold case, nothing changes")
}

// A legacy and a ULID checkpoint that share a shard bucket get one ref each,
// both under the one lowercase spelling, through writes, packing and updates.
// On a case-insensitive filesystem this is the sequence that used to split the
// later-written checkpoint across two refs.
func TestGitRefsStore_LegacyAndULIDShareOneShardSpelling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := refsStoreWithIgnoreCase(t, "true")
	dir := repoDirOf(t, store)

	refsWrite(t, store, sharingLegacy, "sess-1", "legacy one")
	refsWrite(t, store, foldableULID, "sess-1", "ulid one")
	testutil.RunGit(t, dir, "pack-refs", "--all")
	store = reopenRefsStore(t, dir)
	refsWrite(t, store, foldableULID, "sess-2", "ulid two")
	refsWrite(t, store, sharingLegacy, "sess-2", "legacy two")
	testutil.RunGit(t, dir, "pack-refs", "--all")
	store = reopenRefsStore(t, dir)

	assert.Equal(t, []string{
		"refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B",
		"refs/entire/checkpoints/6b/a1b2c3d4e56b",
	}, checkpointRefNames(t, store))
	for _, cid := range []id.CheckpointID{sharingLegacy, foldableULID} {
		summary, err := store.Read(ctx, cid)
		require.NoError(t, err)
		require.NotNil(t, summary)
		assert.Len(t, summary.Sessions, 2, "%s must keep both sessions on one ref", cid)
	}
}

// commitChain returns n commits in a line, oldest first, made in dir.
func commitChain(t *testing.T, dir string, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := range n {
		testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "version "+string(rune('a'+i)))
		out = append(out, testutil.RunGit(t, dir, "rev-parse", "HEAD")[:40])
	}
	return out
}

// shardRenameRepo is a repository on which git recorded a case-insensitive
// filesystem, and its checkpoint ref listing.
func shardRenameRepo(t *testing.T, ignoreCase string) (dir string, repo func() *git.Repository) {
	t.Helper()
	dir = t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "config", "core.ignorecase", ignoreCase)
	return dir, func() *git.Repository {
		r, err := git.PlainOpen(dir)
		require.NoError(t, err)
		return r
	}
}

func refTips(t *testing.T, dir string) map[string]string {
	t.Helper()
	tips := map[string]string{}
	out := testutil.RunGit(t, dir, "for-each-ref", CheckpointRefPrefix, "--format=%(refname) %(objectname)")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			tips[fields[0]] = fields[1]
		}
	}
	return tips
}

func TestNormalizeShardSpelling_RenamesUppercaseShardRefs(t *testing.T) {
	t.Parallel()
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "packed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, repo := shardRenameRepo(t, "true")
			tip := commitChain(t, dir, 1)[0]
			// A ULID under its uppercase shard, and a legacy ID misfiled under the
			// uppercase spelling because a ULID created the bucket first.
			testutil.RunGit(t, dir, "update-ref", "refs/entire/checkpoints/6B/01M2DCHJCHTR9T9MZTSB7WV76B", tip)
			testutil.RunGit(t, dir, "update-ref", "refs/entire/checkpoints/6B/a1b2c3d4e56b", tip)
			if packed {
				testutil.RunGit(t, dir, "pack-refs", "--all")
			}

			result, err := NormalizeShardSpelling(context.Background(), repo())
			require.NoError(t, err)

			assert.Equal(t, 2, result.Renamed)
			assert.Empty(t, result.Diverged)
			assert.Equal(t, map[string]string{
				"refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B": tip,
				"refs/entire/checkpoints/6b/a1b2c3d4e56b":               tip,
			}, refTips(t, dir), "every ref is renamed, at the commit it held, and nothing is lost")
		})
	}
}

func TestNormalizeShardSpelling_ResolvesSplitsByHistory(t *testing.T) {
	t.Parallel()
	upper := "refs/entire/checkpoints/6B/01M2DCHJCHTR9T9MZTSB7WV76B"
	lower := "refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B"
	tests := []struct {
		name             string
		upperAt, lowerAt int // indexes into a chain of two commits
		want             int
	}{
		{"same commit", 0, 0, 0},
		{"lowercase is newer", 0, 1, 1},
		{"uppercase is newer", 1, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, repo := shardRenameRepo(t, "true")
			chain := commitChain(t, dir, 2)
			// Two spellings of one shard can only coexist as separate refs once
			// packed: write one, pack, write the other, pack.
			testutil.RunGit(t, dir, "update-ref", upper, chain[tt.upperAt])
			testutil.RunGit(t, dir, "pack-refs", "--all")
			testutil.RunGit(t, dir, "update-ref", lower, chain[tt.lowerAt])
			testutil.RunGit(t, dir, "pack-refs", "--all")
			require.Len(t, refTips(t, dir), 2, "precondition: one ref under each spelling")

			result, err := NormalizeShardSpelling(context.Background(), repo())
			require.NoError(t, err)

			assert.Equal(t, 1, result.Renamed)
			assert.Equal(t, map[string]string{lower: chain[tt.want]}, refTips(t, dir),
				"the later version survives under the lowercase name")
		})
	}
}

func TestNormalizeShardSpelling_LeavesDivergedSplitsAlone(t *testing.T) {
	t.Parallel()
	dir, repo := shardRenameRepo(t, "true")
	base := commitChain(t, dir, 1)[0]
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "left")
	left := testutil.RunGit(t, dir, "rev-parse", "HEAD")[:40]
	testutil.RunGit(t, dir, "reset", "--hard", base)
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "right")
	right := testutil.RunGit(t, dir, "rev-parse", "HEAD")[:40]
	require.NotEqual(t, left, right)
	upper := "refs/entire/checkpoints/6B/01M2DCHJCHTR9T9MZTSB7WV76B"
	lower := "refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B"
	testutil.RunGit(t, dir, "update-ref", upper, left)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	testutil.RunGit(t, dir, "update-ref", lower, right)
	testutil.RunGit(t, dir, "pack-refs", "--all")

	result, err := NormalizeShardSpelling(context.Background(), repo())
	require.NoError(t, err)

	assert.Equal(t, 0, result.Renamed)
	assert.Equal(t, []id.CheckpointID{foldableULID}, result.Diverged)
	assert.Equal(t, map[string]string{upper: left, lower: right}, refTips(t, dir),
		"neither version holds the other, so both stay for the user to compare")
}

func TestNormalizeShardSpelling_CaseSensitiveRepoIsLeftAsItIs(t *testing.T) {
	t.Parallel()
	dir, repo := shardRenameRepo(t, "false")
	tip := commitChain(t, dir, 1)[0]
	upper := "refs/entire/checkpoints/6B/01M2DCHJCHTR9T9MZTSB7WV76B"
	testutil.RunGit(t, dir, "update-ref", upper, tip)

	result, err := NormalizeShardSpelling(context.Background(), repo())
	require.NoError(t, err)

	assert.Equal(t, 0, result.Renamed)
	assert.Equal(t, map[string]string{upper: tip}, refTips(t, dir))
}

// A ref that moves between planning and the delete must survive: the delete is
// guarded by the commit the rename copied. The other refs are still renamed.
func TestNormalizeShardSpelling_KeepsARefThatMovedDuringTheRename(t *testing.T) {
	t.Parallel()
	dir, repo := shardRenameRepo(t, "true")
	chain := commitChain(t, dir, 2)
	upper := "refs/entire/checkpoints/6B/01M2DCHJCHTR9T9MZTSB7WV76B"
	lower := "refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B"
	otherUpper := "refs/entire/checkpoints/ZN/01KVBJCWYA4YW6J5M9GP655HZN"
	otherLower := "refs/entire/checkpoints/zn/01KVBJCWYA4YW6J5M9GP655HZN"
	testutil.RunGit(t, dir, "update-ref", upper, chain[0])
	testutil.RunGit(t, dir, "update-ref", otherUpper, chain[0])
	testutil.RunGit(t, dir, "pack-refs", "--all")

	r := repo()
	plan, diverged, err := planShardRenames(context.Background(), r, dir)
	require.NoError(t, err)
	require.Empty(t, diverged)
	require.Len(t, plan, 2)
	testutil.RunGit(t, dir, "update-ref", upper, chain[1], chain[0])
	testutil.RunGit(t, dir, "pack-refs", "--all")

	_, err = applyShardRenames(context.Background(), dir, plan)
	require.Error(t, err, "the guarded delete must refuse a ref that moved")

	tips := refTips(t, dir)
	assert.Equal(t, chain[1], tips[upper], "the moved ref is kept")
	assert.Equal(t, chain[0], tips[lower], "the copy made before it moved is kept too")
	assert.Equal(t, chain[0], tips[otherLower], "a ref that did not move is renamed")
	assert.NotContains(t, tips, otherUpper, "a ref that did not move is renamed")
}
