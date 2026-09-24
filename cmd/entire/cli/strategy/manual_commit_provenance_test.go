package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestStampedTrailer_SkipsInheritedTrailers(t *testing.T) {
	t.Parallel()
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J30")
	fresh := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J31")
	msg := "Squashed\n\nEntire-Checkpoint: " + inherited.String() + "\nEntire-Checkpoint: " + fresh.String() + "\n"

	got, ok := stampedTrailer(msg, []id.CheckpointID{inherited})
	assert.True(t, ok)
	assert.Equal(t, fresh, got)

	_, ok = stampedTrailer("Squashed\n\nEntire-Checkpoint: "+inherited.String()+"\n", []id.CheckpointID{inherited})
	assert.False(t, ok, "an inherited trailer alone is not a stamp")

	got, ok = stampedTrailer(msg, nil)
	assert.True(t, ok)
	assert.Equal(t, inherited, got, "with nothing inherited the first trailer is the stamp, as before")
}

func TestPickCondensationTarget(t *testing.T) {
	t.Parallel()
	a, b, c := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3A"), id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3B"), id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3C")
	existing := map[id.CheckpointID]bool{a: true, b: true}
	exists := func(cpID id.CheckpointID) bool { return existing[cpID] }

	got, ok := pickCondensationTarget([]id.CheckpointID{a}, nil)
	assert.True(t, ok)
	assert.Equal(t, a, got, "a lone trailer is the target without consulting the store")

	got, ok = pickCondensationTarget([]id.CheckpointID{b, a, c}, exists)
	assert.True(t, ok)
	assert.Equal(t, c, got, "the stamped trailer is the one without a checkpoint")

	_, ok = pickCondensationTarget([]id.CheckpointID{b, a}, exists)
	assert.False(t, ok, "only links: nothing to condense into")

	_, ok = pickCondensationTarget(nil, exists)
	assert.False(t, ok)
}

func TestPickCondensationTargetState_RechecksSelectedTarget(t *testing.T) {
	t.Parallel()
	linked := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3A")
	target := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3B")
	targetChecks := 0
	exists := func(cpID id.CheckpointID) bool {
		if cpID == linked {
			return true
		}
		targetChecks++
		return targetChecks > 1
	}

	got, preexisting, found := pickCondensationTargetState([]id.CheckpointID{linked, target}, exists)
	assert.True(t, found)
	assert.Equal(t, target, got)
	assert.True(t, preexisting,
		"a target created after selection must be treated as preexisting before condensation")
}

// The inherited-trailer marker speaks only for a commit on the parent it was
// recorded against, and is consumed by the first post-commit that reads it.
func TestInheritedTrailersMarker_TiedToParentAndConsumed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	parent := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J50")
	ctx := context.Background()

	recordInheritedTrailers(ctx, []id.CheckpointID{inherited})
	require.Empty(t, takeInheritedTrailers(ctx, "0000000000000000000000000000000000000000"), "a commit on another parent is not the one prepared")
	require.Empty(t, takeInheritedTrailers(ctx, parent), "the marker is consumed by the first reader")

	recordInheritedTrailers(ctx, []id.CheckpointID{inherited})
	require.Equal(t, map[id.CheckpointID]bool{inherited: true}, takeInheritedTrailers(ctx, parent))

	recordInheritedTrailers(ctx, []id.CheckpointID{inherited})
	recordInheritedTrailers(ctx, nil)
	require.Empty(t, takeInheritedTrailers(ctx, parent), "a later prepare that inherited nothing clears the marker")
}

// Git discards everything below the scissors line of a `commit -v` message; an
// inherited trailer must land above git's comment block to survive.
func TestAddInheritedCheckpointTrailer_StaysAboveGitComments(t *testing.T) {
	t.Parallel()
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J51")
	msg := "Subject\n\n# Please enter the commit message.\n# ------------------------ >8 ------------------------\ndiff --git a/f b/f\n"
	got := addInheritedCheckpointTrailer(msg, inherited)
	trailerAt := strings.Index(got, "Entire-Checkpoint: "+inherited.String())
	require.GreaterOrEqual(t, trailerAt, 0, "%q", got)
	require.Less(t, trailerAt, strings.Index(got, "# Please enter"), "%q", got)
	require.True(t, strings.HasSuffix(got, "# ------------------------ >8 ------------------------\ndiff --git a/f b/f\n"), "git's block is kept intact: %q", got)

	require.Equal(t, addCheckpointTrailer("Subject\n", inherited), addInheritedCheckpointTrailer("Subject\n", inherited),
		"a message without git comments is unchanged in behaviour")
}
