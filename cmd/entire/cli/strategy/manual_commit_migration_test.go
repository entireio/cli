package strategy

import (
	"context"
	"testing"

	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupMigrationRepo creates a temp repo with an initial commit and returns
// the directory and the initial HEAD hash. The caller must call t.Chdir(dir)
// before invoking migrateShadowBranchIfNeeded (it resolves CWD).
func setupMigrationRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "init.txt", "init")
	testutil.GitAdd(t, dir, "init.txt")
	testutil.GitCommit(t, dir, "initial commit")
	return dir, testutil.GetHeadHash(t, dir)
}

// TestMigrateShadowBranch_ReconcilePath verifies that when HEAD carries the
// session's LastCheckpointID trailer, the reconcile path fires: both BaseCommit
// and AttributionBaseCommit are updated to HEAD, and the old shadow branch is
// left untouched.
func TestMigrateShadowBranch_ReconcilePath(t *testing.T) {
	dir, initHash := setupMigrationRepo(t)
	t.Chdir(dir)

	cpID := checkpointID.MustCheckpointID("abc123def456")

	// Create a commit with the matching Entire-Checkpoint trailer.
	testutil.WriteFile(t, dir, "file.txt", "content")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "add feature\n\nEntire-Checkpoint: abc123def456")
	headHash := testutil.GetHeadHash(t, dir)

	// Set up a shadow branch at the OLD base to verify it is NOT deleted.
	oldShadowName := "entire/" + initHash[:7] + "-"
	testutil.CreateBranch(t, dir, oldShadowName)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &SessionState{
		SessionID:             "test-session",
		BaseCommit:            initHash,
		AttributionBaseCommit: initHash,
		LastCheckpointID:      cpID,
	}

	s := &ManualCommitStrategy{}
	migrated, _, err := s.migrateShadowBranchIfNeeded(context.Background(), repo, state)
	require.NoError(t, err)

	assert.True(t, migrated, "reconcile path should report migrated=true")
	assert.Equal(t, headHash, state.BaseCommit, "BaseCommit should advance to HEAD")
	assert.Equal(t, headHash, state.AttributionBaseCommit, "AttributionBaseCommit should advance to HEAD")

	// Old shadow branch must still exist (not renamed or deleted).
	assert.True(t, testutil.BranchExists(t, dir, oldShadowName), "old shadow branch should be preserved")
}

// TestMigrateShadowBranch_CherryPickedCheckpointDoesNotTriggerReconcile
// verifies that a cherry-picked or rebased commit which *preserves* the
// session's LastCheckpointID trailer (same message, different SHA) does NOT
// fire the reconcile path. Only a reset back to the exact condensed commit
// should reconcile; cherry-pick creates a new SHA and must go through the
// migrate path so AttributionBaseCommit stays pinned.
func TestMigrateShadowBranch_CherryPickedCheckpointDoesNotTriggerReconcile(t *testing.T) {
	dir, initHash := setupMigrationRepo(t)
	t.Chdir(dir)

	cpID := checkpointID.MustCheckpointID("abc123def456")

	// HEAD carries the matching trailer but is a DIFFERENT SHA from the one
	// recorded at condensation time (LastCheckpointCommitHash). This is the
	// cherry-pick / rebase scenario.
	testutil.WriteFile(t, dir, "file.txt", "content")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "cherry-picked commit\n\nEntire-Checkpoint: abc123def456")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &SessionState{
		SessionID:                "test-session-cherry-pick",
		BaseCommit:               initHash,
		AttributionBaseCommit:    "original-pinned-attribution",
		LastCheckpointID:         cpID,
		LastCheckpointCommitHash: "0000000000000000000000000000000000000042", // distinct from HEAD
	}

	s := &ManualCommitStrategy{}
	_, reconciled, err := s.migrateShadowBranchIfNeeded(context.Background(), repo, state)
	require.NoError(t, err)

	assert.False(t, reconciled,
		"cherry-pick preserving the trailer must NOT fire reconcile (HEAD SHA != LastCheckpointCommitHash); reconcile would drop the pinned AttributionBaseCommit and corrupt attribution math for uncondensed shadow-branch work")
	assert.Equal(t, "original-pinned-attribution", state.AttributionBaseCommit,
		"AttributionBaseCommit pin must survive a cherry-picked checkpoint trailer (migrate path preserves it; reconcile would not)")
}

// TestMigrateShadowBranch_ReconcileClearsDivergenceFlag verifies that the reconcile
// path also clears DivergenceNoticeShown. Without this, a session that warned about
// divergence, got reset back to a known checkpoint, and later diverged again would
// stay silent — defeating the show-once-per-divergence semantics.
func TestMigrateShadowBranch_ReconcileClearsDivergenceFlag(t *testing.T) {
	dir, initHash := setupMigrationRepo(t)
	t.Chdir(dir)

	cpID := checkpointID.MustCheckpointID("abc123def456")

	testutil.WriteFile(t, dir, "file.txt", "content")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "add feature\n\nEntire-Checkpoint: abc123def456")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &SessionState{
		SessionID:             "test-session-reconcile-flag",
		BaseCommit:            initHash,
		AttributionBaseCommit: "some-older-commit",
		LastCheckpointID:      cpID,
		DivergenceNoticeShown: true, // was warned about divergence previously
	}

	s := &ManualCommitStrategy{}
	migrated, _, err := s.migrateShadowBranchIfNeeded(context.Background(), repo, state)
	require.NoError(t, err)
	require.True(t, migrated)

	assert.False(t, state.DivergenceNoticeShown,
		"reconcile must clear DivergenceNoticeShown so future divergence can warn again")
}

// TestMigrateShadowBranch_MigratePathPinsAttribution verifies the existing
// migrate path: when HEAD changes but has no matching trailer, BaseCommit
// advances but AttributionBaseCommit stays pinned.
func TestMigrateShadowBranch_MigratePathPinsAttribution(t *testing.T) {
	dir, initHash := setupMigrationRepo(t)
	t.Chdir(dir)

	cpID := checkpointID.MustCheckpointID("abc123def456")

	// Create a second commit WITHOUT any Entire-Checkpoint trailer.
	testutil.WriteFile(t, dir, "file.txt", "content")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "add feature without trailer")
	headHash := testutil.GetHeadHash(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &SessionState{
		SessionID:             "test-session",
		BaseCommit:            initHash,
		AttributionBaseCommit: initHash,
		LastCheckpointID:      cpID,
	}

	s := &ManualCommitStrategy{}
	migrated, _, err := s.migrateShadowBranchIfNeeded(context.Background(), repo, state)
	require.NoError(t, err)

	assert.True(t, migrated, "migrate path should report migrated=true")
	assert.Equal(t, headHash, state.BaseCommit, "BaseCommit should advance to HEAD")
	assert.Equal(t, initHash, state.AttributionBaseCommit, "AttributionBaseCommit should stay pinned")
}

// TestMigrateShadowBranch_DifferentTrailerFromSameSession verifies that when
// HEAD has a DIFFERENT Entire-Checkpoint trailer (not matching LastCheckpointID),
// the migrate path fires instead of reconcile, and AttributionBaseCommit stays pinned.
func TestMigrateShadowBranch_DifferentTrailerFromSameSession(t *testing.T) {
	dir, initHash := setupMigrationRepo(t)
	t.Chdir(dir)

	sessionCpID := checkpointID.MustCheckpointID("abc123def456")

	// Create a commit with a DIFFERENT checkpoint ID.
	testutil.WriteFile(t, dir, "file.txt", "content")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "add feature\n\nEntire-Checkpoint: 111111222222")
	headHash := testutil.GetHeadHash(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &SessionState{
		SessionID:             "test-session",
		BaseCommit:            initHash,
		AttributionBaseCommit: initHash,
		LastCheckpointID:      sessionCpID,
	}

	s := &ManualCommitStrategy{}
	migrated, _, err := s.migrateShadowBranchIfNeeded(context.Background(), repo, state)
	require.NoError(t, err)

	assert.True(t, migrated, "migrate path should fire")
	assert.Equal(t, headHash, state.BaseCommit, "BaseCommit should advance")
	assert.Equal(t, initHash, state.AttributionBaseCommit, "AttributionBaseCommit should stay pinned (migrate, not reconcile)")
}

// TestMigrateShadowBranch_EmptyLastCheckpointID verifies that when the session
// has never been condensed (LastCheckpointID is empty), the reconcile guard is
// skipped and the migrate path fires.
func TestMigrateShadowBranch_EmptyLastCheckpointID(t *testing.T) {
	dir, initHash := setupMigrationRepo(t)
	t.Chdir(dir)

	// Create a commit WITH a checkpoint trailer, but session has no LastCheckpointID.
	testutil.WriteFile(t, dir, "file.txt", "content")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "add feature\n\nEntire-Checkpoint: abc123def456")
	headHash := testutil.GetHeadHash(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &SessionState{
		SessionID:             "test-session",
		BaseCommit:            initHash,
		AttributionBaseCommit: initHash,
		// LastCheckpointID is zero value (empty) - never condensed.
	}

	s := &ManualCommitStrategy{}
	migrated, _, err := s.migrateShadowBranchIfNeeded(context.Background(), repo, state)
	require.NoError(t, err)

	assert.True(t, migrated, "migrate path should fire")
	assert.Equal(t, headHash, state.BaseCommit, "BaseCommit should advance")
	assert.Equal(t, initHash, state.AttributionBaseCommit, "AttributionBaseCommit should stay pinned (migrate path)")
}

// TestMigrateShadowBranch_MultiTrailerHEAD verifies that the reconcile path
// matches when the session's LastCheckpointID appears as the SECOND trailer
// in a commit message (e.g. a squash-merge commit).
func TestMigrateShadowBranch_MultiTrailerHEAD(t *testing.T) {
	dir, initHash := setupMigrationRepo(t)
	t.Chdir(dir)

	cpID := checkpointID.MustCheckpointID("bbb222ccc333")

	// Commit with two Entire-Checkpoint trailers; the session ID matches the second.
	testutil.WriteFile(t, dir, "file.txt", "content")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "squash merge\n\nEntire-Checkpoint: aaa111bbb222\nEntire-Checkpoint: bbb222ccc333")
	headHash := testutil.GetHeadHash(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	state := &SessionState{
		SessionID:             "test-session",
		BaseCommit:            initHash,
		AttributionBaseCommit: initHash,
		LastCheckpointID:      cpID,
	}

	s := &ManualCommitStrategy{}
	migrated, _, err := s.migrateShadowBranchIfNeeded(context.Background(), repo, state)
	require.NoError(t, err)

	assert.True(t, migrated, "reconcile should fire on multi-trailer match")
	assert.Equal(t, headHash, state.BaseCommit, "BaseCommit should advance to HEAD")
	assert.Equal(t, headHash, state.AttributionBaseCommit, "AttributionBaseCommit should advance (reconcile path)")
}
